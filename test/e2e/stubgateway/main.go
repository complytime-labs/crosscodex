// Package main stands in for the LLM gateway in the Tier A E2E suite
// (test/e2e/run-daemon.sh). Tier A runs no LLM analysis, but crosscodexd
// refuses to start unless llm.gateway_url answers, so this answers every
// request with 404 until it is terminated.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <listen-address>\n", os.Args[0])
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              os.Args[1],
		Handler:           http.NotFoundHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to serve on %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
