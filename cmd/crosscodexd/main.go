package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/complytime-labs/crosscodex/internal/version"
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

	info := version.GetInfo()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("crosscodexd %s starting\n", info.Version)

	// TODO: start services here // DevSkim: ignore DS176209 - scaffold placeholder, tracked in backlog

	<-ctx.Done()
	stop()
	fmt.Println("crosscodexd shutting down")
}
