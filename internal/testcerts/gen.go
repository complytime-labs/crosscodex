//go:build ignore

// Command gen generates ephemeral TLS certificates for integration tests.
//
// Usage:
//
//	go run ./internal/testcerts/gen.go <output-dir>
//
// This generates 6 PEM files (CA, server, client — cert + key each) into
// the specified directory. It is invoked by the taskfile's
// test-generate-certs task.
package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <output-dir>\n", os.Args[0])
		os.Exit(1)
	}

	dir := os.Args[1]

	pki, err := testcerts.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate certs: %v\n", err)
		os.Exit(1)
	}

	if err := pki.WriteToDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "write certs: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Certificates written to %s\n", dir)
}
