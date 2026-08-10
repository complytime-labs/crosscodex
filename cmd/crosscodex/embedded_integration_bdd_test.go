//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Embedded daemon startup (S1/S3)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	It("writes a PID file that parses to a live pid and port", func() {
		port, alive := readPIDFile(sharedState.daemon.pidFile)
		Expect(alive).To(BeTrue(), "pid in file should be a live process")
		Expect(port).To(Equal(sharedState.daemon.port))
	})

	It("provisions the embedded tenant (regression of #113)", func() {
		var rows int
		err := sharedState.daemon.resources.dbPool.QueryRow(ctx,
			"SELECT count(*) FROM tenants WHERE tenant_id = $1", embeddedTenantID).Scan(&rows)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(Equal(1))
	})

	It("is healthy over mTLS", func() {
		Expect(healthCheck(ctx, sharedState.client)).To(BeTrue())
	})

	It("has RLS enabled with policies on the tenant-scoped catalog tables", func() {
		for _, table := range []string{"catalogs", "controls"} {
			var rlsEnabled bool
			err := sharedState.daemon.resources.dbPool.QueryRow(ctx,
				"SELECT relrowsecurity FROM pg_class WHERE relname = $1", table).Scan(&rlsEnabled)
			Expect(err).NotTo(HaveOccurred(), "query relrowsecurity for %s", table)
			Expect(rlsEnabled).To(BeTrue(), "RLS should be enabled on %s", table)

			var policyCount int
			err = sharedState.daemon.resources.dbPool.QueryRow(ctx,
				"SELECT count(*) FROM pg_policies WHERE tablename = $1", table).Scan(&policyCount)
			Expect(err).NotTo(HaveOccurred(), "query pg_policies for %s", table)
			Expect(policyCount).To(BeNumerically(">", 0), "expected RLS policies on %s", table)
		}
	})
})

var _ = Describe("readPIDFile error paths", func() {
	// readPIDFile (cmd/crosscodex/connect.go) gates whether connect() reuses a running
	// daemon or starts a new one. A false positive (alive=true for a dead pid)
	// would send the CLI at a daemon that is not there, so each error-return
	// path is exercised here alongside the happy path above.

	It("returns (0,false) when the file is missing", func() {
		port, alive := readPIDFile(filepath.Join(GinkgoT().TempDir(), "absent.pid"))
		Expect(alive).To(BeFalse())
		Expect(port).To(Equal(0))
	})

	It("returns (0,false) for a truncated one-line file", func() {
		path := filepath.Join(GinkgoT().TempDir(), "daemon.pid")
		Expect(os.WriteFile(path, []byte("12345\n"), 0o600)).To(Succeed())
		port, alive := readPIDFile(path)
		Expect(alive).To(BeFalse())
		Expect(port).To(Equal(0))
	})

	It("returns (0,false) for a non-numeric pid", func() {
		path := filepath.Join(GinkgoT().TempDir(), "daemon.pid")
		Expect(os.WriteFile(path, []byte("notapid\n8080\n"), 0o600)).To(Succeed())
		port, alive := readPIDFile(path)
		Expect(alive).To(BeFalse())
		Expect(port).To(Equal(0))
	})

	It("returns (0,false) for a non-numeric port", func() {
		path := filepath.Join(GinkgoT().TempDir(), "daemon.pid")
		Expect(os.WriteFile(path, []byte("12345\nnotaport\n"), 0o600)).To(Succeed())
		port, alive := readPIDFile(path)
		Expect(alive).To(BeFalse())
		Expect(port).To(Equal(0))
	})

	It("returns (port,false) for a well-formed file whose pid is not alive", func() {
		// PID 999999 sits above the usual pid_max on the test container and is
		// exceedingly unlikely to name a live process, so Signal(0) reports it
		// dead. The port is still parsed and returned — this is the case that
		// distinguishes a stale daemon from a malformed file (which returns 0).
		path := filepath.Join(GinkgoT().TempDir(), "daemon.pid")
		Expect(os.WriteFile(path, []byte("999999\n8080\n"), 0o600)).To(Succeed())
		port, alive := readPIDFile(path)
		Expect(alive).To(BeFalse(), "a stale pid must not read as alive")
		Expect(port).To(Equal(8080), "the port must still be parsed from a stale pid file")
	})
})

var _ = Describe("Catalog round-trip (S2)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	It("imports, lists, and reads back a 5-control catalog, and re-imports idempotently", func() {
		data := fixture("minimal-catalog.json")

		resp, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.GetJobId()).NotTo(BeEmpty())
		Expect(resp.GetDocumentId()).NotTo(BeEmpty())
		Expect(resp.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_PENDING))

		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		catalogID := catalogs[0].GetCatalogId()
		Expect(catalogID).NotTo(BeEmpty())

		cat, err := getCatalog(ctx, sharedState.client, catalogID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetControlCount()).To(BeEquivalentTo(5))

		before, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		countBefore := len(before)

		// Re-import identical bytes → same content-hash id → idempotent update.
		_, err = importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())

		cat, err = getCatalog(ctx, sharedState.client, catalogID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetControlCount()).To(BeEquivalentTo(5), "re-import must not duplicate controls")
		after, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(countBefore), "re-import must not add a catalog row")
	})

	// BUG (surfaced during #114 e2e, not in the original issue enumeration):
	// GetCatalog never reports the imported format. catalogRecordToProto
	// (internal/catalog/service.go) omits the Catalog.Format enum field
	// (it only writes the string into Provenance.LineageMetadata["format"]), and
	// ParseCatalog never populates prov.Format. Net: GetCatalog().GetFormat()
	// is always CATALOG_FORMAT_UNSPECIFIED.
	PIt("reports the imported catalog format (OSCAL) on read-back", func() {
		data := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())
		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		cat, err := getCatalog(ctx, sharedState.client, catalogs[0].GetCatalogId())
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_OSCAL))
	})

	It("currently returns UNSPECIFIED format on read-back (characterization)", func() {
		// TRIPWIRE for the format bug above: when this fails, the bug is fixed —
		// flip the Pending spec above to active and delete this tripwire.
		data := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())
		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		cat, err := getCatalog(ctx, sharedState.client, catalogs[0].GetCatalogId())
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED))
	})
})

var _ = Describe("Graceful shutdown (S4)", func() {
	It("removes the PID file, closes the pool, and stops serving", func() {
		ctx := context.Background()
		state, cleanup := startTestDaemon()
		defer cleanup() // idempotent: stop() guards nil; RemoveAll ignores missing dirs

		pool := state.daemon.resources.dbPool
		pidFile := state.daemon.pidFile
		client := state.client

		Expect(healthCheck(ctx, client)).To(BeTrue())

		state.daemon.stop()
		state.daemon = nil // prevent double-stop in cleanup

		Expect(pidFile).NotTo(BeAnExistingFile())
		Expect(healthCheck(ctx, client)).To(BeFalse(), "old client should fail after shutdown")

		var one int
		err := pool.QueryRow(ctx, "SELECT 1").Scan(&one)
		Expect(err).To(HaveOccurred(), "query on a closed pool must error")
	})
})

var _ = Describe("Error paths", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	It("rejects malformed JSON and persists no catalog", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("invalid.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "broken")
		Expect(err).To(HaveOccurred())
		// Malformed JSON fails the content-sniff (isOSCALJSON cannot unmarshal
		// it), so the service routes to the structurer path rather than the
		// OSCAL parser. Embedded mode wires no structurer, so the observed
		// rejection is FailedPrecondition "structurer not configured" — not a
		// parse error. See the isOSCALJSON sniff and the unconfigured-structurer
		// branch in ParseCatalog (internal/catalog/service.go).
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("structurer not configured"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("rejects OSCAL-declared input with no catalog key and persists no catalog", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("no-catalog-key.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "nokey")
		Expect(err).To(HaveOccurred())
		// Valid JSON without a top-level "catalog" key also fails the sniff
		// (isOSCALJSON finds no "catalog" member), so — like malformed JSON —
		// it routes to the unconfigured structurer. The OSCAL format hint is
		// ignored; only the content decides. See isOSCALJSON and the
		// unconfigured-structurer branch in ParseCatalog (internal/catalog/service.go).
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("structurer not configured"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("returns an actionable error when the database is unreachable", func() {
		cfg := &config.Config{}
		cfg.Database.DSN = "postgres://invalid:invalid@127.0.0.1:1/nope?sslmode=disable"
		_, _, err := buildEmbeddedService(ctx, cfg, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp("(?i)databas|connect|dial"))
	})
})

var _ = Describe("Edge cases (green)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	It("#14 rejects an empty catalog (no controls)", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("empty-catalog.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "empty")
		Expect(err).To(HaveOccurred())
		// Empty-but-valid OSCAL is rejected: oscal.ErrNoControls (pkg/oscal),
		// surfaced as CodeInvalidArgument in ParseCatalog (internal/catalog/service.go).
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("no controls"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("#18 GetCatalog with an unknown id returns NotFound", func() {
		_, err := getCatalog(ctx, sharedState.client, "does-not-exist")
		Expect(err).To(HaveOccurred())
		// GetCatalog store not-found passthrough (internal/catalog/service.go).
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
	})

	It("#19 GetCatalog with an empty id returns InvalidArgument", func() {
		_, err := getCatalog(ctx, sharedState.client, "")
		Expect(err).To(HaveOccurred())
		// Rejected in the gateway GetCatalog handler before the backend:
		// internal/gateway/catalog.go.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
	})

	It("#22 parses OSCAL bytes even when the format hint is UNSPECIFIED", func() {
		// The isOSCALJSON content-sniff (internal/catalog/service.go) overrides the format hint.
		_, err := importCatalog(ctx, sharedState.client, fixture("minimal-catalog.json"),
			pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "sniffed")
		Expect(err).NotTo(HaveOccurred())
		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		cat, err := getCatalog(ctx, sharedState.client, catalogs[0].GetCatalogId())
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetControlCount()).To(BeEquivalentTo(5))
	})

	It("#26 rejects duplicate control IDs and persists no catalog", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("duplicate-control-ids.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "dupes")
		Expect(err).To(HaveOccurred())
		// validateItems "duplicate control ID" (internal/catalog/service.go) → CodeInvalidArgument.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("#27 rejects an enhancement whose id equals its parent control's id", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("self-ref-parent.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "selfref")
		Expect(err).To(HaveOccurred())
		// self-ref-parent.json nests an enhancement reusing its parent's id. OSCAL
		// flattening emits every control with an empty ParentID (parts derive a
		// distinct childID), so the two same-id controls collapse to the duplicate-ID
		// branch of validateItems, not its self-referential-parent branch. That branch
		// requires a ControlItem with ParentID == ID, which the importer never produces
		// and which is covered directly by the validateItems unit/property tests. Assert
		// the concrete message so this spec verifies which branch actually fired.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("duplicate control ID"))
	})

	It("#23 SubmitDocument with source_uri is unsupported in embedded mode", func() {
		resp, err := sharedState.client.SubmitDocument(ctx, connectrpc.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_SourceUri{SourceUri: "file:///tmp/nope.json"},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
		}))
		_ = resp
		Expect(err).To(HaveOccurred())
		// localIngestionBackend.ConvertDocument rejects source_uri
		// (cmd/crosscodex/embedded.go).
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeUnimplemented))
	})
})

var _ = Describe("Edge cases — TLS client (green)", func() {
	It("#24 connectClientWithTLS fails on a truncated CA cert", func() {
		dir := GinkgoT().TempDir()
		badCA := filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(badCA, []byte("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"), 0o600)).To(Succeed())
		// pkiPaths shape (embeddedTLSPaths / pkiPaths in cmd/crosscodex/embedded.go);
		// only CACert is exercised before the parse fails.
		paths := pkiPaths(dir)
		paths.CACert = badCA
		_, err := connectClientWithTLS("localhost:1", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp("(?i)ca cert|parse|certificate"))
	})
})

var _ = Describe("Bug records (Pending correct + active tripwire)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	// --- #2: non-OSCAL structuring not implemented ---
	PIt("#2 non-OSCAL input produces a catalog or a clear 'raw unsupported' error", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("no-catalog-key.json"),
			pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "raw")
		// Correct behavior: either success, or a clearly-worded unsupported
		// error — NOT a bare Unimplemented and NOT the current FailedPrecondition
		// "structurer not configured" leak. Both accepted outcomes are encoded
		// so promoting this spec does not spuriously fail on the clear-error fix.
		if err != nil {
			Expect(connectrpc.CodeOf(err)).NotTo(Equal(connectrpc.CodeUnimplemented))
			Expect(connectrpc.CodeOf(err)).NotTo(Equal(connectrpc.CodeFailedPrecondition))
			Expect(err.Error()).NotTo(ContainSubstring("structurer not configured"))
		}
	})

	It("#2 TRIPWIRE: non-OSCAL input currently returns FailedPrecondition", func() {
		// TRIPWIRE for bug #2: when this fails, non-OSCAL structuring works —
		// flip the PIt above to active and delete this. See the unconfigured-structurer
		// branch in ParseCatalog (internal/catalog/service.go).
		_, err := importCatalog(ctx, sharedState.client, fixture("no-catalog-key.json"),
			pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "raw")
		Expect(err).To(HaveOccurred())
		// Observed: the ParseCatalog structurer branch (internal/catalog/service.go) returns
		// CodeFailedPrecondition "structurer not configured" when format is
		// UNSPECIFIED and the OSCAL sniff fails.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("structurer not configured"))
	})

	// --- #7: metadata-only, zero-chunk upload ---
	PIt("#7 a metadata-only upload returns a clear 'empty document' error", func() {
		_, err := importCatalog(ctx, sharedState.client, nil,
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "empty-upload")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp("(?i)empty"))
	})

	It("#7 TRIPWIRE: metadata-only upload currently fails without an empty-doc message", func() {
		// TRIPWIRE for bug #7: when this fails, empty uploads are handled
		// explicitly — flip the PIt above and delete this. An empty upload is
		// stored as an empty blob, then fails the OSCAL content-sniff and
		// routes to the unconfigured structurer — the same generic path as any
		// non-OSCAL input, with no empty-specific message. See the structurer
		// branch in ParseCatalog (internal/catalog/service.go).
		_, err := importCatalog(ctx, sharedState.client, nil,
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "empty-upload")
		Expect(err).To(HaveOccurred())
		// Strict inverse of the PIt above: exactly one can pass. The PIt wants a
		// message naming an "empty" condition; the current message names none —
		// it is the generic structurer leak. Pinning the concrete text ensures
		// this tripwire flips (and prompts promotion of the PIt) the moment the
		// bug is fixed.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("structurer not configured"))
		Expect(err.Error()).NotTo(MatchRegexp("(?i)empty"))
	})

	// --- #3: edited re-import creates a new row instead of updating ---
	PIt("#3 re-importing an edited catalog updates the same logical catalog", func() {
		orig := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, orig, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v1")
		Expect(err).NotTo(HaveOccurred())
		before, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(before).To(HaveLen(1))

		edited := append([]byte(nil), orig...) // caller edits a control's prose
		edited = bytes.Replace(edited, []byte("Separate duties of individuals."), []byte("Separate duties of individuals across roles."), 1)
		_, err = importCatalog(ctx, sharedState.client, edited, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v2")
		Expect(err).NotTo(HaveOccurred())

		// Correct: still one logical catalog (updated), not two.
		after, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(1))
	})

	It("#3 TRIPWIRE: an edited re-import currently creates a second catalog row", func() {
		// TRIPWIRE for bug #3: when this fails, edits update in place —
		// flip the PIt above and delete this. Content-hash id:
		// generateCatalogID (internal/catalog/service.go).
		orig := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, orig, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v1")
		Expect(err).NotTo(HaveOccurred())
		edited := bytes.Replace(append([]byte(nil), orig...),
			[]byte("Separate duties of individuals."), []byte("Separate duties of individuals across roles."), 1)
		_, err = importCatalog(ctx, sharedState.client, edited, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v2")
		Expect(err).NotTo(HaveOccurred())
		after, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(2))
	})

	// --- #6: failed parse leaves an orphan blob ---
	PIt("#6 a failed parse leaves no orphan blob in storage", func() {
		store := sharedState.daemon.resources.storage
		before, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())

		_, _ = importCatalog(ctx, sharedState.client, fixture("invalid.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "orphan")

		after, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(len(before)), "failed import must not leak a blob")
	})

	It("#6 TRIPWIRE: a failed parse currently leaves a blob behind", func() {
		// TRIPWIRE for bug #6: when this fails, failed imports clean up their
		// blob — flip the PIt above and delete this. Blob Put precedes parse in
		// localIngestionBackend.ConvertDocument (cmd/crosscodex/embedded.go).
		store := sharedState.daemon.resources.storage
		before, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		_, _ = importCatalog(ctx, sharedState.client, fixture("invalid.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "orphan")
		after, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(len(after)).To(BeNumerically(">", len(before))) // PIN: confirm the leak is observable
	})
})

var _ = Describe("Bug record #4 — startup health vs DB reachability", func() {
	PIt("#4 health reflects database reachability", func() {
		ctx := context.Background()
		state, cleanup := startTestDaemon()
		defer cleanup()
		state.daemon.resources.dbPool.Close()
		// Correct: with the DB pool closed, health should report unhealthy.
		Expect(healthCheck(ctx, state.client)).To(BeFalse())
	})

	It("#4 TRIPWIRE: health stays green after the DB pool is closed", func() {
		// TRIPWIRE for bug #4: when this fails, the health gate touches the DB —
		// flip the PIt above and delete this. Gate uses the gateway Health RPC,
		// not dbAdminBackend.HealthCheck: healthCheck (cmd/crosscodex/connect.go).
		ctx := context.Background()
		state, cleanup := startTestDaemon()
		defer cleanup()
		state.daemon.resources.dbPool.Close()
		Expect(healthCheck(ctx, state.client)).To(BeTrue()) // PIN: confirm health ignores the closed pool
	})
})

var _ = Describe("Bug record #9 — ListCatalogs tenant scoping", func() {
	// No active tripwire: exposing the missing tenant filter needs a SECOND
	// tenant, which is out of #114 scope ([H]). This Pending spec is the
	// durable record for parity with #2–#7. PGStore.ListCatalogs
	// (internal/catalog/store.go) has no WHERE tenant_id; it relies on RLS,
	// which the embedded owner pool bypasses.
	PIt("#9 ListCatalogs returns only the current tenant's catalogs", func() {
		// Correct behavior asserted once a second tenant harness exists:
		// import under tenant A and tenant B, then ListCatalogs as A returns
		// only A's catalogs. Requires cross-tenant scaffolding (out of scope).
		Fail("requires a second-tenant harness — see Known bugs out of scope")
	})
})
