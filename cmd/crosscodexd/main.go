package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func printUsage() {
	fmt.Print(`CrossCodex Daemon

Usage:
  crosscodexd [command]

Available Commands:
  version     Print version information

Running crosscodexd with no arguments starts the daemon.
Use Ctrl+C or SIGTERM to stop.
`)
}

func printVersion() {
	info := version.GetInfo()
	fmt.Printf("crosscodexd %s (commit: %s, built: %s, go: %s, %s/%s)\n",
		info.Version, info.GitCommit, info.BuildDate,
		info.GoVersion, info.OS, info.Arch)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			printVersion()
			return
		case "help", "--help", "-h":
			printUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
			fmt.Fprintln(os.Stderr, `Run "crosscodexd help" for usage.`)
			os.Exit(1)
		}
	}

	os.Exit(run())
}

// run executes the daemon and returns the process exit code. Deferred
// cleanup runs before returning, so main can safely call os.Exit on the
// result without skipping it.
func run() int {
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

	rt, err := bootstrap(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		return 1
	}
	defer rt.close()

	if err := rt.graphService.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "start graph service: %v\n", err)
		return 1
	}
	defer func() {
		if err := rt.graphService.Stop(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "stop graph service: %v\n", err)
		}
	}()

	// FOLLOW-UP(#128): gateway HTTP server, pipeline.Service, the analyzer
	// registry, and the LLM worker are not wired here — only the graph
	// materialization subscriber runs as a live component.
	fmt.Println("crosscodexd graph service started")

	<-ctx.Done()
	stop()
	fmt.Println("crosscodexd shutting down")
	return 0
}
