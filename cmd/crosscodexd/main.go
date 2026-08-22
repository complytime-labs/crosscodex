package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func printUsage() {
	fmt.Print(`CrossCodex Daemon

Usage:
  crosscodexd [--role ROLE] [command]

Flags:
  --role ROLE   Service role to run: all, gateway, pipeline, worker, graph
                (also: analysis, synthesis -- aliases for pipeline)
                Overrides the "role" config value / CROSSCODEX_ROLE env var.
                Defaults to "all".

Available Commands:
  version     Print version information

Running crosscodexd with no arguments starts the daemon with role "all".
Use Ctrl+C or SIGTERM to stop.
`)
}

func printVersion() {
	info := version.GetInfo()
	fmt.Printf("crosscodexd %s (commit: %s, built: %s, go: %s, %s/%s)\n",
		info.Version, info.GitCommit, info.BuildDate,
		info.GoVersion, info.OS, info.Arch)
}

// effectiveRole returns the role to run: the CLI flag if set, otherwise the
// config-loaded value.
func effectiveRole(flagRole, cfgRole string) string {
	if flagRole != "" {
		return flagRole
	}
	return cfgRole
}

func main() {
	flags := flag.NewFlagSet("crosscodexd", flag.ContinueOnError)
	flags.Usage = printUsage
	role := flags.String("role", "", "service role to run (all, gateway, worker, graph)")

	if err := flags.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	if flags.NArg() > 0 {
		switch flags.Arg(0) {
		case "version", "--version":
			printVersion()
			return
		case "help", "--help", "-h":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", flags.Arg(0))
			fmt.Fprintln(os.Stderr, `Run "crosscodexd help" for usage.`)
			os.Exit(1)
		}
	}

	os.Exit(run(*role))
}

// run executes the daemon and returns the process exit code. Deferred
// cleanup runs before returning, so main can safely call os.Exit on the
// result without skipping it.
func run(roleFlag string) int {
	info := version.GetInfo()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("crosscodexd %s starting\n", info.Version)

	loader := config.NewLoader()
	cfg, err := loader.Load(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load configuration: %v\n", err)
		return 1
	}

	role := effectiveRole(roleFlag, cfg.Role)
	if role != cfg.Role {
		canonical, err := config.ResolveRole(role)
		if err != nil {
			fmt.Fprintf(os.Stderr, "--role: %v\n", err)
			return 1
		}
		cfg.Role = canonical
	}

	rt, err := bootstrap(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		return 1
	}
	defer rt.close()

	if err := rt.start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "start %s role: %v\n", cfg.Role, err)
		return 1
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rt.stop(stopCtx); err != nil {
			fmt.Fprintf(os.Stderr, "stop %s role: %v\n", cfg.Role, err)
		}
	}()

	fmt.Printf("crosscodexd %s role started\n", cfg.Role)

	<-ctx.Done()
	stop()
	fmt.Println("crosscodexd shutting down")
	return 0
}
