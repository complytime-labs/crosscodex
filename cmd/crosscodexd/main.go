package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("crosscodexd starting")

	// TODO: start services here // DevSkim: ignore DS176209 — scaffold placeholder, tracked in backlog

	<-ctx.Done()
	stop()
	fmt.Println("crosscodexd shutting down")
}
