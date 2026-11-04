//go:build ignore

// Command gen generates and verifies ephemeral TLS certificates for
// integration tests.
//
// Usage:
//
//	go run ./internal/testcerts/gen.go <output-dir>
//	go run ./internal/testcerts/gen.go --verify <output-dir>
//
// Without --verify, makes <output-dir> hold a valid set of 6 PEM files (CA,
// server, client — cert + key each) plus a SHA-256 fingerprint file. A set
// that already verifies is kept; otherwise a new set replaces the whole
// directory atomically (see testcerts.Ensure), so concurrent runs and
// interrupted runs never leave a mixed or partial set. <output-dir> must be
// dedicated to the certificates. It is invoked by the taskfile's
// generate-certs task.
//
// With --verify, checks the set is parseable, chains to the CA, has matching
// keys, the required SANs and fingerprint, and at least a day of validity
// left. Exits 0 on success, 1 on failure. Used by the taskfile's
// generate-certs status check.
package main

import (
	"fmt"
	"os"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

func main() {
	opts := testcerts.EnsureOptions{
		Verify:   testcerts.GenerateRequirements(),
		Generate: testcerts.Generate,
	}

	if len(os.Args) == 3 && os.Args[1] == "--verify" {
		dir := os.Args[2]
		if err := testcerts.VerifyDirWith(dir, opts.Verify); err != nil {
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
	generated, err := testcerts.Ensure(dir, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ensure certs: %v\n", err)
		os.Exit(1)
	}
	if !generated {
		fmt.Printf("Certificates in %s are valid; kept\n", dir)
		return
	}
	fmt.Printf("Certificates written to %s\n", dir)
}
