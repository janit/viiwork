// Command viiwork is a viiwork 2 node: one process per machine that runs its
// models, joins the mesh and serves the API. `viiwork alias ...` manages the
// mesh's model aliases through a node's API.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/janit/viiwork/v2/internal/aliascli"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/cost"
	"github.com/janit/viiwork/v2/internal/node"
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

const defaultConfigPath = "/etc/viiwork/viiwork.yaml"

type runEnv struct {
	stdout, stderr io.Writer
	lookupEnv      func(string) (string, bool)
	hostname       func() (string, error)
}

func main() {
	os.Exit(run(os.Args[1:], runEnv{stdout: os.Stdout, stderr: os.Stderr, lookupEnv: os.LookupEnv, hostname: os.Hostname}))
}

// run is the whole command; it returns the exit code.
func run(args []string, env runEnv) int {
	if len(args) > 0 && args[0] == "alias" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return aliascli.Run(ctx, args[1:], aliascli.Env{
			Stdout: env.stdout, Stderr: env.stderr, LookupEnv: env.lookupEnv,
			Hostname: env.hostname, Client: &http.Client{}, ReadFile: os.ReadFile,
		})
	}

	// The config file is the only input: v1's --section.key overrides are gone
	// (P6 Decision 4), so a v1 invocation fails here, loudly.
	fs := flag.NewFlagSet("viiwork", flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	configPath := fs.String("config", defaultConfigPath, "path to viiwork.yaml")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(env.stderr, "usage: viiwork [--config path] [--version]\n       viiwork alias <command> ...\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.stderr, "viiwork: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *showVersion {
		fmt.Fprintln(env.stdout, version)
		return 0
	}

	cost.LoadDotEnv(".env")
	cfg, err := config.Load(*configPath, env.lookupEnv)
	if err != nil {
		// A v1 file's error already names docs/migrating-to-v2.md.
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}
	n, err := node.New(cfg, node.Options{
		ConfigPath: *configPath, Version: version, Log: env.stdout,
		LookupEnv: env.lookupEnv, Hostname: env.hostname,
	})
	if err != nil {
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				_ = n.Reload() // errors are logged by Reload
			}
		}
	}()
	if err := n.Run(ctx); err != nil {
		fmt.Fprintf(env.stderr, "viiwork: %v\n", err)
		return 1
	}
	return 0
}
