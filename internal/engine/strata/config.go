package strata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
)

// Upstream's own defaults (serve/server.py, v0.1.42).
const (
	defaultModelName = "qwen3.8-flash-next"
	parallelMax      = 8 // PARALLEL_MAX: the engine's batch window
)

// maxConfigSize bounds what is read from models[].path. A real config is
// about 1 KB; every other engine's path is a model of many gigabytes, and one
// of those left in place must not be read into the node to find that out.
const maxConfigSize = 1 << 20

// fileConfig is the part of Strata's JSON config this engine checks. Strata
// has no flag for the model name, the context or the slot count, so the node's
// numbers and the file's can disagree, and nothing but this would say so
// before a backend has spent minutes loading.
//
// Values stay raw where upstream is lenient about their type, so that each is
// read the way upstream will read it; a nil value is an absent key.
type fileConfig struct {
	ModelName   json.RawMessage
	Aliases     json.RawMessage
	Args        []string
	Parallel    json.RawMessage
	LazyLoad    json.RawMessage
	IdleUnloadS json.RawMessage
	APIKey      json.RawMessage
}

// readConfigFile reads models[].path, refusing anything that cannot be a
// Strata JSON config before reading it.
func readConfigFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("strata: models[].path must be Strata's JSON config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigSize {
		return nil, fmt.Errorf("strata: models[].path %s must be Strata's JSON config, not the model: a regular file of at most %d bytes", path, maxConfigSize)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("strata: models[].path must be Strata's JSON config: %w", err)
	}
	// Upstream reads the file as utf-8-sig: Notepad writes a byte order mark.
	return bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), nil
}

func readConfig(path string) (fileConfig, error) {
	raw, err := readConfigFile(path)
	if err != nil {
		return fileConfig{}, err
	}
	// A map, not struct tags: Go matches struct keys without regard to case
	// and upstream's dict lookups do not, so "Parallel" must not count.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return fileConfig{}, fmt.Errorf("strata: %s is not a Strata JSON config: %w", path, err)
	}
	c := fileConfig{
		ModelName: keys["model_name"], Aliases: keys["aliases"], Parallel: keys["parallel"],
		LazyLoad: keys["lazy_load"], IdleUnloadS: keys["idle_unload_s"], APIKey: keys["api_key"],
	}
	if a, ok := keys["args"]; ok {
		if err := json.Unmarshal(a, &c.Args); err != nil {
			return fileConfig{}, fmt.Errorf(`strata: %s: "args" must be a list of strings: %w`, path, err)
		}
	}
	return c, nil
}

// names is every name the server answers with: model_name, then aliases,
// which upstream takes as a list or as one comma-separated string.
func (c fileConfig) names() ([]string, error) {
	names := []string{defaultModelName}
	if c.ModelName != nil {
		if err := json.Unmarshal(c.ModelName, &names[0]); err != nil || isNull(c.ModelName) {
			return nil, errors.New(`"model_name" must be a string`)
		}
	}
	if len(c.Aliases) == 0 || isNull(c.Aliases) {
		return names, nil
	}
	var list []string
	if err := json.Unmarshal(c.Aliases, &list); err != nil {
		var one string
		if err := json.Unmarshal(c.Aliases, &one); err != nil {
			return nil, errors.New(`"aliases" must be a list of names or one comma-separated string`)
		}
		list = strings.Split(one, ",")
	}
	for _, a := range list {
		if a = strings.TrimSpace(a); a != "" {
			names = append(names, a)
		}
	}
	return names, nil
}

// maxContext is the engine's --max-context from "args": the flag and its value
// as two args. --max-context=N is refused, because Strata v0.1.42 reads only
// the two-argument form: its server would take the default for that file and
// its engine exits on an unknown argument, so the backend would never start.
// Given twice it is refused: upstream's server reads the first and an engine
// reads the last, so no single number describes the file.
func (c fileConfig) maxContext() (int, error) {
	const flag = "--max-context"
	var values []string
	for i, a := range c.Args {
		if strings.HasPrefix(a, flag+"=") {
			return 0, fmt.Errorf(`"args" writes %s as one argument: Strata reads only the two-argument form, "--max-context", "N"`, a)
		}
		if a == flag && i+1 < len(c.Args) {
			values = append(values, c.Args[i+1])
		}
	}
	switch len(values) {
	case 0:
		return 0, errors.New(`"args" has no --max-context with a value: the node cannot tell what context the backend will serve`)
	case 1:
	default:
		return 0, errors.New(`"args" gives --max-context more than once`)
	}
	n, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, fmt.Errorf(`--max-context %q in "args" is not a number`, values[0])
	}
	return n, nil
}

// slots is how many requests the server will run at once, as far as the file
// says. Upstream uses "parallel" only when it is a whole number above 1, caps
// it at parallelMax, and otherwise serves one. When "args" carry --batch or
// --slots upstream ignores "parallel" and the engine's own flag decides; that
// is reported as not known, and /slots tells the node the real count.
func (c fileConfig) slots() (n int, known bool) {
	if slices.Contains(c.Args, "--batch") || slices.Contains(c.Args, "--slots") {
		return 0, false
	}
	if json.Unmarshal(c.Parallel, &n) != nil || n <= 1 {
		return 1, true
	}
	return min(n, parallelMax), true
}

func (c fileConfig) lazy() bool { return string(bytes.TrimSpace(c.LazyLoad)) == "true" }

// idleUnload reports whether the server would unload itself. Upstream takes
// float(value or 0) and then anything non-zero as on, so true is one second
// and a negative number is on too.
func (c fileConfig) idleUnload() bool {
	switch v := string(bytes.TrimSpace(c.IdleUnloadS)); v {
	case "", "null", "false", `""`:
		return false
	default:
		f, err := strconv.ParseFloat(strings.Trim(v, `" `), 64)
		return err != nil || f != 0
	}
}

// hasAPIKey reports whether the file sets a key. With one, /health stays an
// open 200 while /slots and every request answer 401.
func (c fileConfig) hasAPIKey() bool {
	switch string(bytes.TrimSpace(c.APIKey)) {
	case "", "null", `""`, "false":
		return false
	}
	return true
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// check compares the file with what the node decided. Every error names the
// key in the JSON that is wrong.
func (c fileConfig) check(s engine.Spec) error {
	names, err := c.names()
	if err != nil {
		return err
	}
	if !slices.Contains(names, s.Name) {
		return fmt.Errorf(`the file names the model %s, not %q: set "model_name" to it or add it to "aliases"`,
			quoted(names), s.Name)
	}
	ctx, err := c.maxContext()
	if err != nil {
		return err
	}
	if ctx != s.Context {
		return fmt.Errorf(`--max-context %d in "args" differs from models[].context %d`, ctx, s.Context)
	}
	if got, known := c.slots(); known && got != s.Parallel {
		return fmt.Errorf(`"parallel" gives %d slot(s) but models[].parallel is %d`, got, s.Parallel)
	}
	if c.lazy() {
		return errors.New(`"lazy_load" must be off: an unloaded server is an unhealthy backend to the node`)
	}
	if c.idleUnload() {
		return errors.New(`"idle_unload_s" must be 0: a server that unloads itself is an unhealthy backend to the node`)
	}
	if c.hasAPIKey() {
		return errors.New(`"api_key" must not be set: the node probes and forwards without a key, and the backend listens on loopback only`)
	}
	return nil
}

// quoted lists names each in its own quotes, so a name with a comma in it
// cannot read as two.
func quoted(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, ", ")
}
