// Package aliascli is the `viiwork alias` command. It talks to a node only
// through the node's HTTP API, so it works the same against a docker node, a
// native node, or --node on another machine.
package aliascli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	pollEvery     = 250 * time.Millisecond
	memberTimeout = time.Second
	secretBytes   = 32
)

const usage = `usage: viiwork alias <command> [arguments] [flags]

commands:
  ls                                    list aliases as the node resolves them
  set <name> <target> [--fallback <model>]... [--force]
                                        point an alias at a model
  rm <name>                             delete an alias
  history <name>                        show an alias's versions, newest first
  revert <name>                         swap an alias back to its previous version
  export                                print the whole alias table as JSON
  import <file>                         write every live alias from an exported table

flags, before or after the arguments:
  --node host:port      the node's API (default 127.0.0.1:<api.port> from --config, else 127.0.0.1:8086)
  --config path         viiwork.yaml, read only for api.port and mesh.secret_env
  --secret-env NAME     variable holding the mesh secret (default mesh.secret_env, else VIIWORK_MESH_SECRET)
  --wait                after a write, report how many members have it (default true)
  --timeout duration    how long to wait for them (default 5s)
`

// Env is everything the command touches outside itself.
type Env struct {
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	Hostname  func() (string, error)
	Client    *http.Client
	ReadFile  func(string) ([]byte, error)
}

// commands maps each command to its number of positional arguments.
var commands = map[string]int{"ls": 0, "set": 2, "rm": 1, "history": 1, "revert": 1, "export": 0, "import": 1}

type cli struct {
	env       Env
	node      string
	secretEnv string
	signer    *meshauth.Signer
	wait      bool
	timeout   time.Duration
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// Run runs one alias command; args are the words after "alias". It returns the
// exit code: 0 on success, 1 when a request failed, 2 on a usage error.
func Run(ctx context.Context, args []string, env Env) int {
	env = withDefaults(env)
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}
	cmd := args[0]
	want, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(env.Stderr, "unknown command %q\n%s", cmd, usage)
		return 2
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	node := fs.String("node", "", "")
	configPath := fs.String("config", "", "")
	secretEnv := fs.String("secret-env", "", "")
	wait := fs.Bool("wait", true, "")
	timeout := fs.Duration("timeout", 5*time.Second, "")
	var fallbacks stringList
	var force bool
	if cmd == "set" {
		fs.Var(&fallbacks, "fallback", "")
		fs.BoolVar(&force, "force", false, "")
	}
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		fmt.Fprintf(env.Stderr, "%s: %v\n%s", cmd, err, usage)
		return 2
	}
	if len(pos) != want {
		fmt.Fprintf(env.Stderr, "%s takes %d argument(s)\n%s", cmd, want, usage)
		return 2
	}

	c := &cli{env: env, wait: *wait, timeout: *timeout}
	port := config.Defaults().API.Port
	c.secretEnv = config.DefaultMeshSecretEnv
	if *configPath != "" {
		data, err := env.ReadFile(*configPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "reading %s: %v\n", *configPath, err)
			return 2
		}
		cfg, err := config.Parse(data)
		if err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", *configPath, err)
			return 2
		}
		port = cfg.API.Port
		if cfg.Mesh.SecretEnv != "" {
			c.secretEnv = cfg.Mesh.SecretEnv
		}
	}
	c.node = *node
	if c.node == "" {
		c.node = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if *secretEnv != "" {
		c.secretEnv = *secretEnv
	}
	if v, ok := env.LookupEnv(c.secretEnv); ok && v != "" {
		secret, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(secret) != secretBytes {
			fmt.Fprintf(env.Stderr, "%s must be the standard base64 encoding of a %d-byte mesh secret\n", c.secretEnv, secretBytes)
			return 2
		}
		host, err := env.Hostname()
		if err != nil || host == "" {
			host = "unknown"
		}
		if c.signer, err = meshauth.NewSigner(secret, "viiwork-cli@"+host); err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", c.secretEnv, err)
			return 2
		}
	}

	switch cmd {
	case "ls":
		return c.list(ctx)
	case "set":
		req := meshapi.AliasWriteRequest{Target: pos[1], Fallbacks: append([]string{}, fallbacks...), Force: force}
		return c.write(ctx, http.MethodPut, meshapi.AliasPath(pos[0]), pos[0], req)
	case "rm":
		return c.write(ctx, http.MethodDelete, meshapi.AliasPath(pos[0]), pos[0], nil)
	case "revert":
		return c.write(ctx, http.MethodPost, meshapi.AliasRevertPath(pos[0]), pos[0], nil)
	case "history":
		return c.history(ctx, pos[0])
	case "export":
		return c.export(ctx)
	default:
		return c.importTable(ctx, pos[0])
	}
}

func withDefaults(env Env) Env {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.LookupEnv == nil {
		env.LookupEnv = os.LookupEnv
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.Client == nil {
		env.Client = &http.Client{}
	}
	if env.ReadFile == nil {
		env.ReadFile = os.ReadFile
	}
	return env
}

// parseInterleaved lets flags come before, between or after positional
// arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// apiError is a node's refusal.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.message, e.status) }

// call sends one request to the node, signing writes when a secret is set,
// and decodes a 2xx body into out.
func (c *cli) call(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.node+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && c.signer != nil {
		if _, err := c.signer.SignRequest(req, body); err != nil { // last: it covers the request
			return err
		}
	}
	resp, err := c.env.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e meshapi.ErrorResponse
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return &apiError{status: resp.StatusCode, message: msg}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// fail prints a failed request with a hint for the usual authorisation
// mistakes, and returns exit code 1.
func (c *cli) fail(err error) int {
	fmt.Fprintf(c.env.Stderr, "error: %v\n", err)
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusUnauthorized:
			fmt.Fprintf(c.env.Stderr, "this mesh is secured: set %s to the mesh secret\n", c.secretEnv)
		case http.StatusForbidden:
			fmt.Fprintln(c.env.Stderr, "this mesh is open: alias writes work only on the node's own machine, without --node")
		}
	}
	return 1
}

func (c *cli) list(ctx context.Context) int {
	var resp meshapi.AliasesResponse
	if err := c.call(ctx, http.MethodGet, meshapi.PathAliases, nil, &resp); err != nil {
		return c.fail(err)
	}
	tw := tabwriter.NewWriter(c.env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTARGET\tFALLBACKS\tRESOLVED\tSTATE\tUPDATED\tBY")
	for _, in := range resp.Aliases {
		fallbacks, resolved := "-", "-"
		if len(in.Fallbacks) > 0 {
			fallbacks = strings.Join(in.Fallbacks, ",")
		}
		if in.Resolved != nil {
			resolved = *in.Resolved
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", in.Name, in.Target, fallbacks, resolved, in.State, in.UpdatedAt, in.UpdatedBy)
	}
	tw.Flush()
	return 0
}

func (c *cli) write(ctx context.Context, method, path, name string, req any) int {
	var b meshapi.AliasBroadcast
	if err := c.call(ctx, method, path, req, &b); err != nil {
		return c.fail(err)
	}
	if b.Entry.Deleted {
		fmt.Fprintf(c.env.Stdout, "%s deleted (ver %d)\n", name, b.Entry.Ver)
	} else {
		fmt.Fprintf(c.env.Stdout, "%s -> %s (ver %d)\n", name, b.Entry.Target, b.Entry.Ver)
	}
	if c.wait {
		c.converge(ctx, name, b.Entry)
	}
	return 0
}

// converge polls the members until all of them show the written entry or the
// timeout passes, then prints how many do (P5 Decision 10). It never changes
// the exit code: the write itself already succeeded.
func (c *cli) converge(ctx context.Context, name string, e meshapi.AliasEntry) {
	deadline := time.Now().Add(c.timeout)
	var have, of int
	for {
		addrs, err := c.aliveMembers(ctx)
		if err != nil {
			fmt.Fprintf(c.env.Stdout, "convergence unknown: %v\n", err)
			return
		}
		have, of = c.round(ctx, addrs, name, e), len(addrs)
		if have == of || time.Now().Add(pollEvery).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
	if e.Deleted {
		fmt.Fprintf(c.env.Stdout, "%s deleted (ver %d): %d/%d alive members\n", name, e.Ver, have, of)
		return
	}
	fmt.Fprintf(c.env.Stdout, "%s -> %s (ver %d): %d/%d alive members\n", name, c.resolvedOnNode(ctx, name, e.Target), e.Ver, have, of)
}

// aliveMembers is the API address of every alive node member with a status.
func (c *cli) aliveMembers(ctx context.Context) ([]string, error) {
	var cluster meshapi.ClusterResponse
	if err := c.call(ctx, http.MethodGet, meshapi.PathCluster, nil, &cluster); err != nil {
		return nil, err
	}
	var addrs []string
	for _, m := range cluster.Members {
		if m.State == meshapi.MemberAlive && m.Role == meshapi.RoleNode && m.Status != nil {
			addrs = append(addrs, net.JoinHostPort(m.Addr, strconv.Itoa(m.Status.APIPort)))
		}
	}
	return addrs, nil
}

// round asks every member, in parallel, whether it shows the written entry.
func (c *cli) round(ctx context.Context, addrs []string, name string, e meshapi.AliasEntry) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	have := 0
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if c.memberHas(ctx, addr, name, e) {
				mu.Lock()
				have++
				mu.Unlock()
			}
		}(addr)
	}
	wg.Wait()
	return have
}

func (c *cli) memberHas(ctx context.Context, addr, name string, e meshapi.AliasEntry) bool {
	ctx, cancel := context.WithTimeout(ctx, memberTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+meshapi.PathAliases, nil)
	if err != nil {
		return false
	}
	resp, err := c.env.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var list meshapi.AliasesResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&list) != nil {
		return false
	}
	for _, in := range list.Aliases {
		if in.Name != name {
			continue
		}
		if e.Deleted {
			return false
		}
		at, err := time.Parse(time.RFC3339Nano, in.UpdatedAt)
		return err == nil && at.UnixMilli() == e.TS && in.UpdatedBy == e.By
	}
	return e.Deleted
}

// resolvedOnNode is the model the node resolves the alias to now, or the
// target when it resolves to nothing or cannot be asked.
func (c *cli) resolvedOnNode(ctx context.Context, name, target string) string {
	var resp meshapi.AliasesResponse
	if err := c.call(ctx, http.MethodGet, meshapi.PathAliases, nil, &resp); err != nil {
		return target
	}
	for _, in := range resp.Aliases {
		if in.Name == name && in.Resolved != nil {
			return *in.Resolved
		}
	}
	return target
}

func (c *cli) table(ctx context.Context) (meshapi.AliasTable, error) {
	var t meshapi.AliasTable
	err := c.call(ctx, http.MethodGet, meshapi.PathAliases+"?table=1", nil, &t)
	return t, err
}

func (c *cli) history(ctx context.Context, name string) int {
	t, err := c.table(ctx)
	if err != nil {
		return c.fail(err)
	}
	e, ok := t.Aliases[name]
	if !ok {
		fmt.Fprintf(c.env.Stderr, "alias %s not found\n", name)
		return 1
	}
	if e.Deleted {
		fmt.Fprintf(c.env.Stdout, "ver %d  %s  %s  deleted\n", e.Ver, formatTS(e.TS), e.By)
	} else {
		fmt.Fprintf(c.env.Stdout, "ver %d  %s  %s  -> %s%s\n", e.Ver, formatTS(e.TS), e.By, e.Target, fallbackSuffix(e.Fallbacks))
	}
	for _, v := range e.History {
		fmt.Fprintf(c.env.Stdout, "       %s  %s  -> %s%s\n", formatTS(v.TS), v.By, v.Target, fallbackSuffix(v.Fallbacks))
	}
	return 0
}

func (c *cli) export(ctx context.Context) int {
	t, err := c.table(ctx)
	if err != nil {
		return c.fail(err)
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.env.Stdout, "%s\n", raw)
	return 0
}

// importTable writes every live alias of an exported table with force, since
// the targets may not be loaded yet, and skips tombstones, so a stale export
// cannot delete anything (P5 Decision 11).
func (c *cli) importTable(ctx context.Context, path string) int {
	data, err := c.env.ReadFile(path)
	if err != nil {
		fmt.Fprintf(c.env.Stderr, "reading %s: %v\n", path, err)
		return 1
	}
	var t meshapi.AliasTable
	if err := json.Unmarshal(data, &t); err != nil {
		fmt.Fprintf(c.env.Stderr, "%s: %v\n", path, err)
		return 1
	}
	if t.V != meshapi.AliasTableVersion {
		fmt.Fprintf(c.env.Stderr, "%s: alias table version %d, expected %d\n", path, t.V, meshapi.AliasTableVersion)
		return 1
	}
	var names []string
	for name, e := range t.Aliases {
		if !e.Deleted {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var failed []string
	for _, name := range names {
		e := t.Aliases[name]
		req := meshapi.AliasWriteRequest{Target: e.Target, Fallbacks: append([]string{}, e.Fallbacks...), Force: true}
		if err := c.call(ctx, http.MethodPut, meshapi.AliasPath(name), req, nil); err != nil {
			fmt.Fprintf(c.env.Stderr, "import %s: %v\n", name, err)
			failed = append(failed, name)
		}
	}
	fmt.Fprintf(c.env.Stdout, "imported %d of %d\n", len(names)-len(failed), len(names))
	if len(failed) > 0 {
		fmt.Fprintf(c.env.Stderr, "failed: %s\n", strings.Join(failed, ", "))
		return 1
	}
	return 0
}

// formatTS matches the nodes' updated_at: RFC 3339, UTC, milliseconds.
func formatTS(ts int64) string {
	return time.UnixMilli(ts).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func fallbackSuffix(fallbacks []string) string {
	if len(fallbacks) == 0 {
		return ""
	}
	return " [fallbacks: " + strings.Join(fallbacks, ", ") + "]"
}
