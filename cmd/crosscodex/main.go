package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/version"
)

func printUsage() {
	fmt.Print(`CrossCodex CLI

Usage:
  crosscodex <command>

Available Commands:
  version     Print version information

Use "crosscodex help" for more information.
`)
}

func printVersion() {
	info := version.GetInfo()
	fmt.Printf("crosscodex %s (commit: %s, built: %s, go: %s, %s/%s)\n",
		info.Version, info.GitCommit, info.BuildDate,
		info.GoVersion, info.OS, info.Arch)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	switch os.Args[1] {
	case "version", "--version":
		printVersion()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		fmt.Fprintln(os.Stderr, `Run "crosscodex help" for usage.`)
		os.Exit(1)
	}
}
