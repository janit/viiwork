// Command viiwork-accept checks a viiwork 2 host during and after conversion:
// its config before v1 stops, the mesh ports, join and departure timings, and
// inference, saturation and aliases on the running mesh. It only reads node
// state and sends inference requests; it never starts, stops or configures
// anything.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"

	// Acceptance validates a config file for the node that will run it, so it
	// must know the same engines the node does.
	"github.com/janit/viiwork/v2/internal/config"
	_ "github.com/janit/viiwork/v2/internal/engine/llamacpp"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

type runEnv struct {
	Stdout    io.Writer
	Stderr    io.Writer
	LookupEnv func(string) (string, bool)
	Accept    accept.Env
	Ctx       context.Context
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(os.Args[1:], runEnv{
		Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.LookupEnv,
		Accept: accept.DefaultEnv(), Ctx: ctx,
	})
	stop()
	os.Exit(code)
}

// command is one subcommand: its flags, a one-line summary, and the action,
// which returns the exit code.
type command struct {
	name    string
	args    string // required flags, for the usage line
	summary string
	flags   func(fs *flag.FlagSet) func(env runEnv, out *output) int
}

var commands = []command{
	{"ports serve", "--addr <mesh address>", "echo on the gossip port over tcp and udp for a probe", portsServe},
	{"ports probe", "--host <machine>", "check this machine reaches host's api and gossip ports", portsProbe},
	{"config", "--file <viiwork.yaml>", "validate a v2 config and summarise what it runs", configCmd},
	{"join", "--observer <host:port> --node <name>", "time until a node and its models join the mesh", joinCmd},
	{"ready", "--node <host:port>", "time until every model of a node has a healthy backend", readyCmd},
	{"gone", "--observer <host:port> --node <name>", "time until the mesh stops listing a node as alive", goneCmd},
	{"models", "--node <host:port>", "content, tool-call and pin checks per model", modelsCmd},
	{"saturate", "--node <host:port> --model <name>", "overflow a model's local slots and check the mesh takes it", saturateCmd},
	{"alias", "--entry <host:port|url> --alias <name> --expect-model <name>", "check an alias resolves through every entry point", aliasCmd},
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "usage: viiwork-accept <command> [flags]\n\ncommands:\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprintf(w, "\nRun viiwork-accept <command> --help for its flags. Exit 0: every check passed; 1: a check failed; 2: usage error.\n")
}

// errUsage marks a usage error the action has already described.
var errUsage = errors.New("usage")

func run(args []string, env runEnv) int {
	if env.Ctx == nil {
		env.Ctx = context.Background()
	}
	if len(args) == 0 {
		usage(env.Stderr)
		return 2
	}
	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help", "help":
		usage(env.Stdout)
		return 0
	case "--version", "version":
		fmt.Fprintln(env.Stdout, version)
		return 0
	case "ports":
		if len(rest) == 0 || (rest[0] != "serve" && rest[0] != "probe") {
			fmt.Fprintln(env.Stderr, "viiwork-accept ports: want serve or probe")
			usage(env.Stderr)
			return 2
		}
		name, rest = "ports "+rest[0], rest[1:]
	}
	for _, c := range commands {
		if c.name == name {
			return c.run(rest, env)
		}
	}
	fmt.Fprintf(env.Stderr, "viiwork-accept: unknown command %q\n", name)
	usage(env.Stderr)
	return 2
}

func (c command) run(args []string, env runEnv) int {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	var parseErr strings.Builder
	fs.SetOutput(&parseErr)
	jsonOut := fs.Bool("json", false, "write the report as JSON")
	action := c.flags(fs)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			c.help(env.Stdout, fs)
			return 0
		}
		fmt.Fprintf(env.Stderr, "viiwork-accept %s: %v\n", c.name, err)
		c.help(env.Stderr, fs)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "viiwork-accept %s: unexpected argument %q\n", c.name, fs.Arg(0))
		c.help(env.Stderr, fs)
		return 2
	}
	out := &output{env: env, json: *jsonOut, command: c, fs: fs}
	return action(env, out)
}

func (c command) help(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "usage: viiwork-accept %s %s [flags]\n\n%s\n\nflags:\n", c.name, c.args, c.summary)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fs.VisitAll(func(f *flag.Flag) {
		kind, text := flag.UnquoteUsage(f)
		if _, isList := f.Value.(*stringList); isList {
			kind = "value (repeatable)"
		}
		def := ""
		if f.DefValue != "" && f.DefValue != "false" {
			def = " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(tw, "  --%s %s\t%s%s\n", f.Name, kind, text, def)
	})
	tw.Flush()
}

// output writes a command's report and turns it into the exit code.
type output struct {
	env     runEnv
	json    bool
	command command
	fs      *flag.FlagSet
}

// usageError describes a bad flag combination and returns exit code 2.
func (o *output) usageError(format string, a ...any) int {
	fmt.Fprintf(o.env.Stderr, "viiwork-accept %s: %s\n", o.command.name, fmt.Sprintf(format, a...))
	o.command.help(o.env.Stderr, o.fs)
	return 2
}

// require is a usage error naming the first empty required flag.
func (o *output) require(flags map[string]string, order ...string) error {
	for _, name := range order {
		if strings.TrimSpace(flags[name]) == "" {
			o.usageError("--%s is required", name)
			return errUsage
		}
	}
	return nil
}

func (o *output) report(r accept.Report) int {
	var err error
	if o.json {
		err = r.WriteJSON(o.env.Stdout)
	} else {
		err = r.WriteText(o.env.Stdout)
	}
	if err != nil {
		fmt.Fprintf(o.env.Stderr, "viiwork-accept: writing report: %v\n", err)
		return 1
	}
	if r.Pass() {
		return 0
	}
	return 1
}

// logf is where a long-running command's progress goes: stdout, unless the
// report there is JSON.
func (o *output) logf(format string, a ...any) {
	w := o.env.Stdout
	if o.json {
		w = o.env.Stderr
	}
	fmt.Fprintf(w, format+"\n", a...)
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func portsServe(fs *flag.FlagSet) func(runEnv, *output) int {
	addr := fs.String("addr", "", "this machine's mesh address to listen on")
	port := fs.Int("port", 7946, "port to echo on, tcp and udp")
	serveFor := fs.Duration("for", 10*time.Minute, "stop after this long")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"addr": *addr}, "addr") != nil {
			return 2
		}
		ip, err := netip.ParseAddr(*addr)
		if err != nil {
			return o.usageError("--addr %q is not an IP address", *addr)
		}
		start := env.Accept.Now()
		target := netip.AddrPortFrom(ip, uint16(*port)).String()
		r := accept.Report{Command: "ports serve", Target: target, Started: start}
		ctx, cancel := context.WithTimeout(env.Ctx, *serveFor)
		defer cancel()
		err = accept.ServePorts(ctx, ip, *port, o.logf)
		c := accept.Check{Name: "echo tcp+udp " + target, Pass: err == nil, Elapsed: env.Accept.Now().Sub(start)}
		if err != nil {
			c.Detail = err.Error()
		} else {
			c.Detail = "stopped"
		}
		r.Checks = []accept.Check{c}
		return o.report(r)
	}
}

func portsProbe(fs *flag.FlagSet) func(runEnv, *output) int {
	host := fs.String("host", "", "the machine to probe")
	apiPort := fs.Int("api-port", 8086, "API port, checked by tcp connect")
	gossipPort := fs.Int("gossip-port", 7946, "gossip port, checked by tcp and udp echo")
	timeout := fs.Duration("timeout", 5*time.Second, "limit per check")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"host": *host}, "host") != nil {
			return 2
		}
		return o.report(accept.ProbePorts(env.Ctx, env.Accept, *host, *apiPort, *gossipPort, *timeout))
	}
}

func configCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	file := fs.String("file", "", "the v2 viiwork.yaml to check")
	modelsRoot := fs.String("models-root", "", "host directory that /models/ paths in the file refer to")
	dummySecret := fs.Bool("dummy-secret", false, "validate a secured mesh without its secret (a random key, never printed)")
	requireMesh := fs.String("require-mesh", "", "fail unless the mesh mode is this: secured or open")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"file": *file}, "file") != nil {
			return 2
		}
		switch *requireMesh {
		case "", "secured", "open":
		default:
			return o.usageError("--require-mesh must be secured or open, got %q", *requireMesh)
		}
		lookup := env.LookupEnv
		if *dummySecret {
			lookup = withDummySecret(*file, lookup)
		}
		sum, r := accept.SummarizeConfig(*file, lookup, *modelsRoot, *requireMesh)
		if o.json {
			var s *accept.ConfigSummary
			if sum.Node != "" || len(sum.Models) > 0 {
				s = &sum
			}
			return o.reportWith(r, struct {
				accept.Report
				Summary *accept.ConfigSummary `json:"summary"`
			}{r, s})
		}
		code := o.report(r)
		if r.Checks[0].Pass {
			writeSummary(env.Stdout, sum)
		}
		return code
	}
}

// reportWith writes v as the JSON report and exits by r.
func (o *output) reportWith(r accept.Report, v any) int {
	enc := jsonEncoder(o.env.Stdout)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(o.env.Stderr, "viiwork-accept: writing report: %v\n", err)
		return 1
	}
	if r.Pass() {
		return 0
	}
	return 1
}

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

// withDummySecret answers the file's secret variable with a random valid key
// when it is unset and the file declares a secured mesh. An open mesh gets no
// key, since a secret there fails validation.
func withDummySecret(path string, lookup func(string) (string, bool)) func(string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return lookup
	}
	cfg, err := config.Parse(data)
	if err != nil || cfg.Mesh.Open {
		return lookup
	}
	name := cfg.Mesh.SecretEnv
	if name == "" {
		name = config.DefaultMeshSecretEnv
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	dummy := base64.StdEncoding.EncodeToString(key)
	return func(k string) (string, bool) {
		if k == name {
			if v, ok := lookup(k); ok && v != "" {
				return v, ok
			}
			return dummy, true
		}
		return lookup(k)
	}
}

func writeSummary(w io.Writer, s accept.ConfigSummary) {
	node := s.Node
	if node == "" {
		node = "(hostname)"
	}
	fmt.Fprintf(w, "\nnode %s  state_dir %s  network %s  mesh %s  api %d  gossip %d\n",
		node, s.StateDir, s.Network, s.Mesh, s.APIPort, s.GossipPort)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tENGINE\tGPUS\tBACKENDS\tSLOTS\tCTX/SLOT\tSTARTUP\tWEIGHTS")
	for _, m := range s.Models {
		gpus := make([]string, len(m.GPUs))
		for i, g := range m.GPUs {
			gpus[i] = fmt.Sprint(g)
		}
		gpuText := strings.Join(gpus, ",")
		if gpuText == "" {
			gpuText = "cpu"
		}
		startup := "default"
		if m.StartupTimeout > 0 {
			startup = m.StartupTimeout.String()
		}
		weights := "-"
		if m.WeightsBytes > 0 {
			weights = fmt.Sprintf("%.1f GiB", float64(m.WeightsBytes)/(1<<30))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", m.Name, m.Engine, gpuText, m.Backends, m.Slots, m.CtxPerSlot, startup, weights)
	}
	tw.Flush()
}

func joinCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	observer := fs.String("observer", "", "a node already in the mesh, host:port")
	node := fs.String("node", "", "name of the node expected to join")
	expect := fs.String("expect", "", "comma-separated models the node must list, e.g. a,b")
	timeout := fs.Duration("timeout", 15*time.Second, "give up after this long")
	poll := fs.Duration("poll", 250*time.Millisecond, "time between polls")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"observer": *observer, "node": *node}, "observer", "node") != nil {
			return 2
		}
		return o.report(accept.WaitJoin(env.Ctx, env.Accept, *observer, *node, splitList(*expect), *timeout, *poll))
	}
}

func readyCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	node := fs.String("node", "", "the node's API, host:port")
	timeout := fs.Duration("timeout", 60*time.Minute, "give up after this long")
	poll := fs.Duration("poll", 2*time.Second, "time between polls")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"node": *node}, "node") != nil {
			return 2
		}
		return o.report(accept.WaitReady(env.Ctx, env.Accept, *node, *timeout, *poll))
	}
}

func goneCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	observer := fs.String("observer", "", "a node that stays in the mesh, host:port")
	node := fs.String("node", "", "name of the node expected to go")
	expectState := fs.String("expect-state", "any", "state the node must settle in: any, left or dead")
	timeout := fs.Duration("timeout", 120*time.Second, "give up after this long")
	poll := fs.Duration("poll", 250*time.Millisecond, "time between polls")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"observer": *observer, "node": *node}, "observer", "node") != nil {
			return 2
		}
		switch *expectState {
		case "any", "left", "dead":
		default:
			return o.usageError("--expect-state must be any, left or dead, got %q", *expectState)
		}
		return o.report(accept.WaitGone(env.Ctx, env.Accept, *observer, *node, *expectState, *timeout, *poll))
	}
}

func modelsCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	node := fs.String("node", "", "the node's API, host:port")
	var models stringList
	fs.Var(&models, "model", "model to check; default every model the node lists")
	via := fs.String("via", "", "another node's API to send the pinned request through")
	timeout := fs.Duration("timeout", 10*time.Minute, "limit per request")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"node": *node}, "node") != nil {
			return 2
		}
		return o.report(accept.CheckModels(env.Ctx, env.Accept, *node, models, accept.ModelCheckOptions{Via: *via, Timeout: *timeout}))
	}
}

func saturateCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	node := fs.String("node", "", "the node's API, host:port")
	model := fs.String("model", "", "the model to saturate")
	extra := fs.Int("extra", 4, "requests beyond the node's local slots")
	maxTokens := fs.Int("max-tokens", 200, "max_tokens per request")
	timeout := fs.Duration("timeout", 10*time.Minute, "limit per request")
	return func(env runEnv, o *output) int {
		if o.require(map[string]string{"node": *node, "model": *model}, "node", "model") != nil {
			return 2
		}
		return o.report(accept.Saturate(env.Ctx, env.Accept, *node, *model, accept.SaturateOptions{Extra: *extra, MaxTokens: *maxTokens, Timeout: *timeout}))
	}
}

func aliasCmd(fs *flag.FlagSet) func(runEnv, *output) int {
	var entries stringList
	fs.Var(&entries, "entry", "entry point to send through: a node's host:port or a gateway URL")
	alias := fs.String("alias", "", "the alias to request")
	expectModel := fs.String("expect-model", "", "the model the alias must resolve to")
	bearerEnv := fs.String("bearer-env", "", "environment variable holding a bearer token for the entry points")
	timeout := fs.Duration("timeout", 10*time.Minute, "limit per request")
	return func(env runEnv, o *output) int {
		if len(entries) == 0 {
			return o.usageError("--entry is required")
		}
		if o.require(map[string]string{"alias": *alias, "expect-model": *expectModel}, "alias", "expect-model") != nil {
			return 2
		}
		e := env.Accept
		if *bearerEnv != "" {
			token, ok := env.LookupEnv(*bearerEnv)
			if !ok || token == "" {
				return o.usageError("--bearer-env: %s is not set", *bearerEnv)
			}
			e.Headers = e.Headers.Clone()
			if e.Headers == nil {
				e.Headers = http.Header{}
			}
			e.Headers.Set("Authorization", "Bearer "+token)
		}
		return o.report(accept.CheckAlias(env.Ctx, e, entries, *alias, *expectModel, accept.ModelCheckOptions{Timeout: *timeout}))
	}
}
