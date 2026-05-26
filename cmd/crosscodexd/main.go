package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/complytime-labs/crosscodex/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		info := version.GetInfo()
		fmt.Printf("crosscodexd %s (commit: %s, built: %s, go: %s, %s/%s)\n",
			info.Version, info.GitCommit, info.BuildDate,
			info.GoVersion, info.OS, info.Arch)
		return
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
