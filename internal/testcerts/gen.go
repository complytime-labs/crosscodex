//go:build ignore

// Command gen generates and verifies ephemeral TLS certificates for
// integration tests.
//
// Usage:
//
//	go run ./internal/testcerts/gen.go <output-dir>
//	go run ./internal/testcerts/gen.go --verify <output-dir>
//
// Without --verify, generates 6 PEM files (CA, server, client — cert +
// key each) plus a SHA-256 fingerprint file into the specified directory.
// It is invoked by the taskfile's generate-certs task.
//
// With --verify, validates that the certs in <output-dir> are parseable,
// not expired, and form a valid CA trust chain. Exits 0 on success, 1 on
// failure. Used by the taskfile's generate-certs status check.
package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--verify" {
		dir := os.Args[2]
		if err := testcerts.VerifyDir(dir); err != nil {
			fmt.Fprintf(os.Stderr, "verify certs: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Certificates in %s are valid\n", dir)
		return
	}

	if len(os.Args) != 2 || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Fprintf(os.Stderr, "usage: %s [--verify] <output-dir>\n", os.Args[0])
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

	if err := testcerts.WriteFingerprint(dir); err != nil {
		fmt.Fprintf(os.Stderr, "write fingerprint: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Certificates written to %s\n", dir)
}
