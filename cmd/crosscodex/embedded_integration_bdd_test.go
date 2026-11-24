//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
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
		Expect(healthCheck(ctx, sharedState.client)).To(Succeed())
	})

	It("has RLS enabled with policies on the tenant-scoped catalog tables", func() {
		for _, table := range []string{"catalogs", "controls", "catalog_containers"} {
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
		Expect(resp.GetCatalogId()).NotTo(BeEmpty())
		Expect(resp.GetDocumentId()).NotTo(BeEmpty())

		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		catalogID := catalogs[0].GetCatalogId()
		Expect(catalogID).To(Equal(resp.GetCatalogId()))

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

	It("stores a signed ingest attestation of the import, verifiable with the PKI directory's public key", func() {
		data := fixture("minimal-catalog.json")

		resp, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())

		r, err := sharedState.daemon.resources.storage.Get(ctx, "attestation/ingest/"+resp.GetDocumentId())
		Expect(err).NotTo(HaveOccurred())
		raw, err := io.ReadAll(r)
		Expect(r.Close()).To(Succeed())
		Expect(err).NotTo(HaveOccurred())

		pubPath := sharedState.fullCfg.Attestation.PublicKeyPath
		Expect(filepath.Base(pubPath)).To(Equal(testcerts.AttestationPubFile), "the key pair defaults to the PKI directory's")
		verifier, err := attestation.NewGenerator(&attestation.FileKeyProvider{PublicKeyPath: pubPath})
		Expect(err).NotTo(HaveOccurred())
		link, err := verifier.Verify(ctx, raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(link.Step).To(Equal("gateway.StreamDocument"))
		digest := storage.ContentHash(data)
		Expect(link.Materials).To(ConsistOf(attestation.Artifact{
			URI: "document://" + embeddedTenantID + "/" + resp.GetDocumentId(), Digest: digest}))
		Expect(link.Products).To(ConsistOf(HaveField("URI", "catalog://"+embeddedTenantID+"/"+resp.GetCatalogId())))
		Expect(link.ByProducts).To(HaveKeyWithValue("content_hash", digest))
	})

	It("reports the imported catalog format (OSCAL) on read-back", func() {
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
})

var _ = Describe("Import-only CLI and refused analysis (#212)", func() {
	var (
		ctx  context.Context
		file string
	)

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
		file = filepath.Join(GinkgoT().TempDir(), "minimal-catalog.json")
		Expect(os.WriteFile(file, fixture("minimal-catalog.json"), 0o600)).To(Succeed())
	})

	runCLI := func(cmd *cobra.Command, args ...string) (string, error) {
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		return out.String(), err
	}

	// listJobIDs follows next_page_token to collect every job ID, and returns
	// the total count the first page reports.
	listJobIDs := func() ([]string, int64) {
		var ids []string
		var total int64
		token := ""
		for page := 0; ; page++ {
			resp, err := sharedState.client.ListJobs(ctx, connectrpc.NewRequest(&pb.ListJobsRequest{
				Options: &pb.ListOptions{Pagination: &pb.Pagination{PageToken: token}},
			}))
			Expect(err).NotTo(HaveOccurred())
			if page == 0 {
				total = resp.Msg.GetPageInfo().GetTotalCount()
			}
			for _, job := range resp.Msg.GetJobs() {
				ids = append(ids, job.GetJobId())
			}
			token = resp.Msg.GetPageInfo().GetNextPageToken()
			if token == "" {
				return ids, total
			}
		}
	}

	It("catalog import prints the catalog ID and the next step", func() {
		out, err := runCLI(newCatalogCmd(sharedState), "import", file)
		Expect(err).NotTo(HaveOccurred())
		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		id := catalogs[0].GetCatalogId()
		Expect(out).To(ContainSubstring("Catalog imported"))
		Expect(out).To(ContainSubstring("Catalog ID: " + id))
		Expect(out).To(ContainSubstring("Next: crosscodex run start --source " + id + " --target <catalog-id>"))
		Expect(out).NotTo(ContainSubstring("Job ID"))
	})

	It("catalog import --json emits catalog_id, document_id and the textless controls only", func() {
		out, err := runCLI(newCatalogCmd(sharedState), "import", file, "--json")
		Expect(err).NotTo(HaveOccurred())
		var got map[string]json.RawMessage
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(HaveKey("catalog_id"))
		Expect(got).To(HaveKey("document_id"))
		Expect(got).To(HaveKey("textless_controls"))
		Expect(got).To(HaveLen(3))
	})

	It("run start fails with an actionable error and leaves the jobs unchanged", func() {
		resp, err := importCatalog(ctx, sharedState.client, fixture("minimal-catalog.json"), pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "minimal")
		Expect(err).NotTo(HaveOccurred())
		beforeIDs, beforeTotal := listJobIDs()

		_, err = runCLI(newRunCmd(sharedState), "start", "--source", resp.GetCatalogId(), "--target", resp.GetCatalogId())
		Expect(err).To(HaveOccurred())
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err.Error()).To(ContainSubstring("crosscodexd"))
		Expect(err.Error()).To(ContainSubstring("cli.endpoint"))

		afterIDs, afterTotal := listJobIDs()
		Expect(afterIDs).To(ConsistOf(beforeIDs))
		Expect(afterTotal).To(Equal(beforeTotal))
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

		Expect(healthCheck(ctx, client)).To(Succeed())

		state.daemon.stop()
		state.daemon = nil // prevent double-stop in cleanup

		Expect(pidFile).NotTo(BeAnExistingFile())
		Expect(healthCheck(ctx, client)).NotTo(Succeed(), "old client should fail after shutdown")

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
		// Malformed JSON fails content detection, so the OSCAL hint is rejected
		// with advice to fix the JSON syntax.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("the document is not valid JSON"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("rejects OSCAL-declared input with no catalog key and persists no catalog", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("no-catalog-key.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "nokey")
		Expect(err).To(HaveOccurred())
		// Valid JSON without a top-level "catalog" key fails content detection,
		// so the OSCAL hint is rejected naming the missing key.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring(`no top-level "catalog" object`))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("returns an actionable error when the database is unreachable", func() {
		cfg := &config.Config{}
		cfg.Database.DSN = "postgres://invalid:invalid@127.0.0.1:1/nope?sslmode=disable"
		_, _, _, err := buildEmbeddedService(ctx, cfg, "", nil)
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

	It("#7 rejects a metadata-only upload as an empty document and stores nothing", func() {
		store := sharedState.daemon.resources.storage
		before, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())

		_, err = importCatalog(ctx, sharedState.client, nil,
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "empty-upload")
		Expect(err).To(HaveOccurred())
		// Refused in the gateway (processDocument, internal/gateway/document.go)
		// before ingestion writes a blob.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("document is empty"))

		after, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(len(before)), "empty upload must not store a blob")
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("rejects format=UNSPECIFIED truncated OSCAL JSON with advice to fix the syntax", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("invalid.json"),
			pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "broken")
		Expect(err).To(HaveOccurred())
		// Auto-detection can't parse it as JSON, so it is not detected as OSCAL.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("not valid JSON"))
		Expect(err.Error()).To(ContainSubstring("fix the JSON syntax"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	It("auto-detects OSCAL format when format=UNSPECIFIED and persists it correctly", func() {
		data := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "auto")
		Expect(err).NotTo(HaveOccurred())
		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		cat, err := getCatalog(ctx, sharedState.client, catalogs[0].GetCatalogId())
		Expect(err).NotTo(HaveOccurred())
		Expect(cat.GetFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_OSCAL))
	})

	It("rejects format=GEMARA when content is detected as OSCAL", func() {
		data := fixture("minimal-catalog.json")
		_, err := importCatalog(ctx, sharedState.client, data, pb.CatalogFormat_CATALOG_FORMAT_GEMARA, "mismatch")
		Expect(err).To(HaveOccurred())
		// OSCAL JSON detected, but client claims Gemara → validation rejects.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("format=CATALOG_FORMAT_GEMARA specified but content appears to be OSCAL JSON"))
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

	It("ListJobMappings with an unknown job id returns NotFound, proving the mappings backend is attached", func() {
		_, err := sharedState.client.ListJobMappings(ctx, connectrpc.NewRequest(&pb.ListJobMappingsRequest{
			JobId: "00000000-0000-0000-0000-000000000000",
		}))
		Expect(err).To(HaveOccurred())
		// CodeUnavailable ("mappings backend not configured") would mean
		// gateway.WithMappingsBackend is missing from the embedded service.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound), "got: %v", err)
	})

	It("ListJobCandidates with an unknown job id returns NotFound, proving the candidates backend is attached", func() {
		_, err := sharedState.client.ListJobCandidates(ctx, connectrpc.NewRequest(&pb.ListJobCandidatesRequest{
			JobId: "00000000-0000-0000-0000-000000000000",
		}))
		Expect(err).To(HaveOccurred())
		// CodeUnavailable ("candidates backend not configured") would mean
		// gateway.WithCandidatesBackend is missing from the embedded service.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound), "got: %v", err)
	})

	It("results summary for an unknown job fails with not-found through the CLI", func() {
		cmd := newResultsCmd(sharedState)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"summary", "00000000-0000-0000-0000-000000000000"})
		err := cmd.ExecuteContext(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
		Expect(err.Error()).NotTo(ContainSubstring("mappings backend not configured"))
		Expect(err.Error()).NotTo(ContainSubstring("control_id is required"))
	})

	It("#19 GetCatalog with an empty id returns InvalidArgument", func() {
		_, err := getCatalog(ctx, sharedState.client, "")
		Expect(err).To(HaveOccurred())
		// Rejected in the gateway GetCatalog handler before the backend:
		// internal/gateway/catalog.go.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
	})

	It("#22 parses OSCAL bytes even when the format hint is UNSPECIFIED", func() {
		// The oscal.DetectCatalogJSON content-sniff (ParseCatalog) overrides the format hint.
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
		_, _, _, err := connectClientWithTLS("localhost:1", paths)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(MatchRegexp("(?i)ca cert|parse|certificate"))
	})
})

var _ = Describe("Bug records", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		truncateCatalogs(ctx, sharedState.daemon.resources.dbPool)
	})

	// --- #2: non-OSCAL input leaked "structurer not configured" ---
	It("#2 non-OSCAL input produces a catalog or a clear 'raw unsupported' error", func() {
		_, err := importCatalog(ctx, sharedState.client, fixture("no-catalog-key.json"),
			pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "raw")
		Expect(err).To(HaveOccurred())
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring("unsupported catalog format"))
		Expect(err.Error()).To(ContainSubstring("Only OSCAL JSON catalogs can be imported"))
		Expect(err.Error()).NotTo(ContainSubstring("structurer"))
		cats, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(cats).To(BeEmpty())
	})

	// --- #3: edited re-import creates a new row instead of updating ---
	It("#3 re-importing an edited catalog under a document is one logical document with two versions", func() {
		orig := fixture("minimal-catalog.json")
		first, err := importCatalogUnder(ctx, sharedState.client, orig, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v1", "minimal")
		Expect(err).NotTo(HaveOccurred())
		Expect(first.GetSupersededCatalogId()).To(BeEmpty())

		edited := bytes.Replace(append([]byte(nil), orig...),
			[]byte("Separate duties of individuals."), []byte("Separate duties of individuals across roles."), 1)
		second, err := importCatalogUnder(ctx, sharedState.client, edited, pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "v2", "minimal")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.GetCatalogId()).NotTo(Equal(first.GetCatalogId()), "an edit is a new immutable version")
		Expect(second.GetSupersededCatalogId()).To(Equal(first.GetCatalogId()))
		Expect(second.GetChangeReport().GetChanged()).To(HaveLen(1))
		Expect(second.GetChangeReport().GetAdded()).To(BeEmpty())
		Expect(second.GetChangeReport().GetRemoved()).To(BeEmpty())

		after, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(2))
		currents := map[string]bool{}
		for _, c := range after {
			Expect(c.GetDocumentSlug()).To(Equal("minimal"), "one logical document")
			currents[c.GetCatalogId()] = c.GetCurrent()
		}
		Expect(currents).To(Equal(map[string]bool{first.GetCatalogId(): false, second.GetCatalogId(): true}))
	})

	// --- #6: failed parse leaves an orphan blob ---
	It("#6 a failed parse leaves no orphan blob in storage", func() {
		store := sharedState.daemon.resources.storage
		before, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())

		_, _ = importCatalog(ctx, sharedState.client, fixture("invalid.json"),
			pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "orphan")

		after, err := store.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(HaveLen(len(before)), "failed import must not leak a blob")
	})
})

var _ = Describe("Bug record #4 — startup health vs DB reachability", func() {
	It("#4 health reflects database reachability", func() {
		ctx := context.Background()
		state, cleanup := startTestDaemon()
		defer cleanup()
		state.daemon.resources.dbPool.Close()
		Expect(healthCheck(ctx, state.client)).To(MatchError(ContainSubstring("database: database unreachable")))
	})
})

var _ = Describe("Bug record #9 — catalog tenant scoping", func() {
	// The embedded pool is the table owner, so RLS does not apply; only the
	// catalog store's tenant predicates keep another tenant's rows out.
	// Embedded auth pins every request to embeddedTenantID, so the second
	// tenant's catalog is written directly through the owner pool.
	It("#9 ListCatalogs and GetCatalog return only the embedded tenant's catalogs", func() {
		ctx := context.Background()
		pool := sharedState.daemon.resources.dbPool
		truncateCatalogs(ctx, pool)

		_, err := importCatalog(ctx, sharedState.client, fixture("minimal-catalog.json"), pb.CatalogFormat_CATALOG_FORMAT_OSCAL, "mine")
		Expect(err).NotTo(HaveOccurred())

		const otherTenant, otherCatalog = "other-tenant", "other-tenant-catalog"
		Expect(dbpkg.EnsureTenant(ctx, pool, otherTenant, "Other Tenant")).To(Succeed())
		Expect(pool.Exec(ctx,
			"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path) VALUES ($1, $2, 'Other', '1.0', 'oscal', 'other')",
			otherCatalog, otherTenant)).To(Succeed())

		catalogs, err := listCatalogs(ctx, sharedState.client)
		Expect(err).NotTo(HaveOccurred())
		Expect(catalogs).To(HaveLen(1))
		Expect(catalogs[0].GetTenantContext().GetTenantId()).To(Equal(embeddedTenantID))

		_, err = getCatalog(ctx, sharedState.client, otherCatalog)
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
	})
})

// The embedded daemon serves AdminService for holds, stats and dry runs, and
// feedback without determinations (#230).
var _ = Describe("Embedded AdminService and feedback", func() {
	const unknownJob = "00000000-0000-0000-0000-000000000000"

	var (
		ctx context.Context
		tc  *pb.TenantContext
	)

	BeforeEach(func() {
		ctx = context.Background()
		tc = &pb.TenantContext{TenantId: embeddedTenantID}
	})

	listHolds := func() []*pb.Hold {
		resp, err := sharedState.adminClient.ListHolds(ctx, connectrpc.NewRequest(&pb.ListHoldsRequest{TenantContext: tc}))
		Expect(err).NotTo(HaveOccurred(), "ListHolds must reach the hold store; Unimplemented means AdminService is not mounted")
		return resp.Msg.GetHolds()
	}

	// A job and a catalog two years old: expired under any realistic tier, so a
	// scan that purged would remove them. The jobs row is inserted directly
	// (the embedded pool is the table owner); both are removed after each spec.
	const expiredJob, expiredCatalog = "expired-job-retention-spec", "expired-catalog-retention-spec"

	rowCount := func(query string, args ...any) int {
		var n int
		Expect(sharedState.daemon.resources.dbPool.QueryRow(ctx, query, args...).Scan(&n)).To(Succeed())
		return n
	}

	// retentionState is every row and object a purge would remove.
	type retentionState struct{ jobs, catalogs, objects int }
	snapshot := func() retentionState {
		objects, err := sharedState.daemon.resources.storage.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		return retentionState{
			jobs:     rowCount("SELECT count(*) FROM jobs WHERE tenant_id = $1", embeddedTenantID),
			catalogs: rowCount("SELECT count(*) FROM catalogs WHERE tenant_id = $1", embeddedTenantID),
			objects:  len(objects),
		}
	}

	BeforeEach(func() {
		pool := sharedState.daemon.resources.dbPool
		Expect(pool.Exec(ctx,
			"INSERT INTO jobs (job_id, tenant_id, status, created_by, created_at) VALUES ($1, $2, 'completed', 'retention-spec', now() - interval '2 years')",
			expiredJob, embeddedTenantID)).To(Succeed())
		Expect(pool.Exec(ctx,
			"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path, created_at) VALUES ($1, $2, 'Expired', '1.0', 'oscal', 'expired', now() - interval '2 years')",
			expiredCatalog, embeddedTenantID)).To(Succeed())
	})

	AfterEach(func() {
		pool := sharedState.daemon.resources.dbPool
		Expect(pool.Exec(ctx, "DELETE FROM jobs WHERE job_id = $1", expiredJob)).To(Succeed())
		Expect(pool.Exec(ctx, "DELETE FROM catalogs WHERE catalog_id = $1", expiredCatalog)).To(Succeed())
	})

	// The embedded config sets no retention tier, so the default policy keeps
	// everything: the seeded rows are reported only under a policy that
	// expires them, and a dry run never removes them.
	It("answers stats and a dry-run scan, reporting expired rows only under a policy that expires them", func() {
		before := snapshot()

		stats, err := sharedState.adminClient.GetRetentionStats(ctx, connectrpc.NewRequest(&pb.GetRetentionStatsRequest{TenantContext: tc}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stats.Msg.GetEligible()).NotTo(BeNil())
		Expect(stats.Msg.GetEligible().GetScanned()).To(BeZero(), "no tier is configured, so nothing is eligible")
		Expect(stats.Msg.GetStreamDrifts()).To(BeEmpty(), "the embedded daemon runs no audit streams")

		scan, err := sharedState.adminClient.ScanRetention(ctx, connectrpc.NewRequest(&pb.ScanRetentionRequest{TenantContext: tc, DryRun: true}))
		Expect(err).NotTo(HaveOccurred())
		Expect(scan.Msg.GetReport().GetScanned()).To(BeZero())

		applied, err := sharedState.adminClient.ApplyRetentionPolicy(ctx, connectrpc.NewRequest(&pb.ApplyRetentionPolicyRequest{
			TenantContext: tc, DryRun: true, Policy: &pb.RetentionPolicy{JobResults: "30d", Catalogs: "30d"},
		}))
		Expect(err).NotTo(HaveOccurred())
		report := applied.Msg.GetReport()
		expiredJobs := rowCount("SELECT count(*) FROM jobs WHERE tenant_id = $1 AND status = 'completed' AND created_at < now() - interval '30 days'", embeddedTenantID)
		Expect(expiredJobs).To(BeNumerically(">=", 1), "the seeded job is expired")
		Expect(report.GetPerClass()).To(HaveKey("job_results"))
		Expect(report.GetPerClass()["job_results"].GetScanned()).To(BeEquivalentTo(expiredJobs))
		Expect(report.GetPerClass()).To(HaveKey("catalogs"))
		Expect(report.GetPerClass()["catalogs"].GetScanned()).To(BeNumerically(">=", 1), "the seeded catalog is expired")
		Expect(report.GetArchived()).To(BeZero(), "a dry run archives nothing")
		Expect(report.GetPurged()).To(BeZero(), "a dry run purges nothing")

		Expect(snapshot()).To(Equal(before), "a dry run must not remove rows or objects")
		Expect(rowCount("SELECT count(*) FROM jobs WHERE job_id = $1", expiredJob)).To(Equal(1))
		Expect(rowCount("SELECT count(*) FROM catalogs WHERE catalog_id = $1", expiredCatalog)).To(Equal(1))
	})

	DescribeTable("refuses a scan that would archive or purge with FailedPrecondition naming what to do, and changes nothing",
		func(call func() error) {
			before := snapshot()
			beforeHolds := listHolds()
			err := call()
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition), "got: %v", err)
			Expect(err).To(MatchError(And(ContainSubstring("--dry-run"), ContainSubstring("database.purge_dsn"))))
			Expect(snapshot()).To(Equal(before), "a refused scan must not remove rows or objects")
			Expect(rowCount("SELECT count(*) FROM jobs WHERE job_id = $1", expiredJob)).To(Equal(1), "the expired job must survive")
			Expect(rowCount("SELECT count(*) FROM catalogs WHERE catalog_id = $1", expiredCatalog)).To(Equal(1), "the expired catalog must survive")
			Expect(listHolds()).To(HaveLen(len(beforeHolds)))
		},
		Entry("ScanRetention", func() error {
			_, err := sharedState.adminClient.ScanRetention(ctx, connectrpc.NewRequest(&pb.ScanRetentionRequest{TenantContext: tc}))
			return err
		}),
		Entry("ApplyRetentionPolicy", func() error {
			_, err := sharedState.adminClient.ApplyRetentionPolicy(ctx, connectrpc.NewRequest(&pb.ApplyRetentionPolicyRequest{
				TenantContext: tc, Policy: &pb.RetentionPolicy{JobResults: "30d", Catalogs: "30d"},
			}))
			return err
		}),
	)

	It("serves the review queue: an unknown job is NotFound, proving the feedback backend is attached", func() {
		_, err := sharedState.client.GetReviewQueue(ctx, connectrpc.NewRequest(&pb.GetReviewQueueRequest{JobId: unknownJob}))
		// CodeUnavailable ("feedback backend not configured") would mean
		// gateway.WithFeedbackBackend is missing from the embedded service.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound), "got: %v", err)
	})

	It("serves votes: SubmitVote for an unknown job reaches the store and is NotFound", func() {
		_, err := sharedState.client.SubmitVote(ctx, connectrpc.NewRequest(&pb.SubmitVoteRequest{
			JobId: unknownJob, MappingId: "map-1", VoteType: pb.VoteType_VOTE_TYPE_AGREE,
		}))
		// PermissionDenied would mean the embedded certificate lacks the
		// reviewer role; Unavailable that no feedback backend is attached.
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound), "got: %v", err)
	})

	// The service refuses before reading the job; one built with
	// determinations would answer NotFound for the unknown job. The refusal is
	// FailedPrecondition, so its text survives the gateway's error policy.
	It("refuses a determination through the CLI's client with FailedPrecondition naming the crosscodexd daemon", func() {
		_, err := sharedState.client.RecordDetermination(ctx, connectrpc.NewRequest(&pb.RecordDeterminationRequest{
			JobId: unknownJob, MappingId: "map-1", DeterminationRationale: "checked",
		}))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition), "got: %v", err)
		Expect(err).To(MatchError(ContainSubstring(
			"mapping determinations need the crosscodexd daemon (role all, or gateway with a graph role)")))
	})
})
