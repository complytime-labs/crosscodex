//go:build ignore

// Command gen generates and verifies the production TLS PKI for the compose
// deployment: the CA, one server certificate and key per TLS service (db, nats,
// litellm, praxis, crosscodexd), each naming only that service, one client
// certificate (clientAuth only), and the in-toto attestation signing keypair.
// Written as PEM files with a fingerprint.
//
// Usage:
//
//	go run ./deploy/certgen/gen.go <output-dir>
//	go run ./deploy/certgen/gen.go --verify <output-dir>
//
// Without --verify, keeps a set that already verifies and otherwise replaces
// the whole directory atomically with a new one (see testcerts.Ensure), so
// concurrent and interrupted runs never leave a mixed or partial set.
// <output-dir> must be dedicated to this material: regeneration discards
// anything else in it. A regeneration mints a new CA, client certificate and
// attestation key. A set in the shared-server-key layout of earlier releases
// (server.pem and server-key.pem) is replaced the same way, after a notice on
// stderr stating the consequences.
//
// With --verify, checks everything Ensure requires to keep a set: the TLS
// chain, key/cert matches, each service's own serverAuth-only certificate
// naming only that service, no key shared between files, no shared
// server.pem, the fingerprint, at least a day of validity left, and a matching
// attestation keypair. Exits 1 with the reason and the fix otherwise.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

var loopback = []net.IP{net.IPv4(127, 0, 0, 1)}

// services are the TLS servers in the compose stack, with the names their
// clients dial. A certificate names only its own service, so one service's key
// cannot impersonate another. localhost and 127.0.0.1 appear only where a host
// process dials the service: the gateway (the CLI, curl) and praxis (the smoke
// test's direct probes of its published port). Adding or changing a name makes
// --verify reject older sets, so the next deploy:certs replaces them.
var services = []testcerts.ServiceIdentity{
	{Name: "db", DNSNames: []string{"db"}},
	{Name: "nats", DNSNames: []string{"nats"}},
	{Name: "litellm", DNSNames: []string{"litellm"}},
	{Name: "praxis", DNSNames: []string{"praxis", "localhost"}, IPs: loopback},
	{Name: "crosscodexd", DNSNames: []string{"crosscodexd", "localhost"}, IPs: loopback},
}

func main() {
	opts := testcerts.EnsureOptions{
		Verify: testcerts.VerifyOptions{
			Services:           services,
			MinRemaining:       24 * time.Hour,
			RequireFingerprint: true,
			VerifyExtra:        testcerts.VerifyAttestationKeypair,
		},
		Generate: func() (*testcerts.PKI, error) {
			return testcerts.GenerateServices("CrossCodex", 3650*24*time.Hour, services)
		},
		WriteExtra: testcerts.WriteAttestationKeypair,
	}

	if len(os.Args) == 3 && os.Args[1] == "--verify" {
		if err := testcerts.VerifyDirWith(os.Args[2], opts.Verify); err != nil {
			fmt.Fprintf(os.Stderr, "verify certs: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Certificates in %s are valid\n", os.Args[2])
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s [--verify] <output-dir>\n", os.Args[0])
		os.Exit(1)
	}
	dir := os.Args[1]

	// A notice, not a gate: Ensure re-verifies under its lock and replaces the
	// set either way. It tells the operator why the CA changes.
	if err := testcerts.VerifyDirWith(dir, opts.Verify); errors.Is(err, testcerts.ErrSharedServerKey) {
		fmt.Fprintf(os.Stderr, "NOTICE: replacing %s: %v\n", dir, err)
	}

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
