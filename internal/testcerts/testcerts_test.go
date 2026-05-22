package testcerts_test

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

func TestGenerate(t *testing.T) {
	t.Parallel()

	pki, err := testcerts.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// All 6 fields must be non-empty PEM.
	fields := map[string][]byte{
		"CACert":     pki.CACert,
		"CAKey":      pki.CAKey,
		"ServerCert": pki.ServerCert,
		"ServerKey":  pki.ServerKey,
		"ClientCert": pki.ClientCert,
		"ClientKey":  pki.ClientKey,
	}
	for name, data := range fields {
		if len(data) == 0 {
			t.Errorf("%s is empty", name)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			t.Errorf("%s is not valid PEM", name)
		}
	}

	// Parse CA cert.
	caBlock, _ := pem.Decode(pki.CACert)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	if !caCert.IsCA {
		t.Error("CA cert IsCA = false, want true")
	}
	if caCert.Subject.CommonName != "CrossCodex Test CA" {
		t.Errorf("CA CN = %q, want %q", caCert.Subject.CommonName, "CrossCodex Test CA")
	}

	// Parse server cert.
	srvBlock, _ := pem.Decode(pki.ServerCert)
	srvCert, err := x509.ParseCertificate(srvBlock.Bytes)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}
	if srvCert.IsCA {
		t.Error("server cert IsCA = true, want false")
	}

	// Check SANs.
	foundDNS := false
	for _, dns := range srvCert.DNSNames {
		if dns == "localhost" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Errorf("server cert DNSNames = %v, want localhost", srvCert.DNSNames)
	}
	foundIP := false
	for _, ip := range srvCert.IPAddresses {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("server cert IPAddresses = %v, want 127.0.0.1", srvCert.IPAddresses)
	}

	// Parse server key — must be ECDSA P-256.
	srvKeyBlock, _ := pem.Decode(pki.ServerKey)
	srvKey, err := x509.ParseECPrivateKey(srvKeyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse server key: %v", err)
	}
	if srvKey.Curve.Params().BitSize != 256 {
		t.Errorf("server key curve bits = %d, want 256", srvKey.Curve.Params().BitSize)
	}

	// Parse client cert.
	cliBlock, _ := pem.Decode(pki.ClientCert)
	cliCert, err := x509.ParseCertificate(cliBlock.Bytes)
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	if cliCert.IsCA {
		t.Error("client cert IsCA = true, want false")
	}
	if cliCert.Subject.CommonName != "crosscodex-test-client" {
		t.Errorf("client CN = %q, want %q", cliCert.Subject.CommonName, "crosscodex-test-client")
	}

	// Verify CA signed both certs.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := srvCert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("server cert not signed by CA: %v", err)
	}
	if _, err := cliCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("client cert not signed by CA: %v", err)
	}
}

func TestWriteToDir(t *testing.T) {
	t.Parallel()

	pki, err := testcerts.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	dir := t.TempDir()
	if err := pki.WriteToDir(dir); err != nil {
		t.Fatalf("WriteToDir() error: %v", err)
	}

	// Check all 6 files exist with correct permissions.
	wantPerms := map[string]os.FileMode{
		"ca.pem":         0644,
		"ca-key.pem":     0600,
		"server.pem":     0644,
		"server-key.pem": 0600,
		"client.pem":     0644,
		"client-key.pem": 0600,
	}
	for name, wantPerm := range wantPerms {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("file %s: %v", name, err)
			continue
		}
		gotPerm := info.Mode().Perm()
		if gotPerm != wantPerm {
			t.Errorf("file %s permissions = %o, want %o", name, gotPerm, wantPerm)
		}
	}
}

func TestWriteToDirCreatesDirectory(t *testing.T) {
	t.Parallel()

	pki, err := testcerts.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "nested", "certs")
	if err := pki.WriteToDir(dir); err != nil {
		t.Fatalf("WriteToDir() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err != nil {
		t.Errorf("ca.pem not created in nested dir: %v", err)
	}
}

func TestTLSHandshake(t *testing.T) {
	t.Parallel()

	pki, err := testcerts.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	// Build CA pool.
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(pki.CACert) {
		t.Fatal("failed to add CA cert to pool")
	}

	// Server TLS config.
	srvCert, err := tls.X509KeyPair(pki.ServerCert, pki.ServerKey)
	if err != nil {
		t.Fatalf("server X509KeyPair: %v", err)
	}
	serverTLS := &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}

	// Start TLS listener.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	// Accept one connection in a goroutine.
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		// Force TLS handshake.
		if tlsConn, ok := conn.(*tls.Conn); ok {
			serverDone <- tlsConn.Handshake()
		} else {
			serverDone <- nil
		}
	}()

	// Client TLS config.
	cliCert, err := tls.X509KeyPair(pki.ClientCert, pki.ClientKey)
	if err != nil {
		t.Fatalf("client X509KeyPair: %v", err)
	}
	clientTLS := &tls.Config{
		Certificates: []tls.Certificate{cliCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS12,
	}

	// Dial the server.
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	// Wait for server handshake.
	if err := <-serverDone; err != nil {
		t.Fatalf("server handshake error: %v", err)
	}

	// Verify we used ECDSA.
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("no peer certificates")
	}
	if _, ok := state.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey); !ok {
		t.Error("server cert is not ECDSA")
	}
}
