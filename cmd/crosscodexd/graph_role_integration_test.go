//go:build integration

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
	"github.com/google/uuid"
)

// graphITReviewerCN is the common name of the reviewer's client certificate.
// The gateway maps it to the reviewer role; the graph role's
// tls.service_identities does not list it.
const graphITReviewerCN = "graph-it-reviewer"

// issueReviewerTLS signs a client certificate for graphITReviewerCN with the
// suite's CA and returns a mutual-TLS client config presenting it.
func issueReviewerTLS() config.TLSConfig {
	certDir := filepath.Dir(testTLSCAPath)
	caPEM, err := os.ReadFile(testTLSCAPath)
	Expect(err).NotTo(HaveOccurred(), "read test CA")
	caKeyPEM, err := os.ReadFile(filepath.Join(certDir, "ca-key.pem"))
	Expect(err).NotTo(HaveOccurred(), "read test CA key")
	block, _ := pem.Decode(caPEM)
	Expect(block).NotTo(BeNil(), "decode test CA")
	caCert, err := x509.ParseCertificate(block.Bytes)
	Expect(err).NotTo(HaveOccurred(), "parse test CA")

	leaf, err := pkigen.GenerateCert(&pkigen.CertKeyPair{CertPEM: caPEM, KeyPEM: caKeyPEM, Cert: caCert},
		pkigen.WithOrganization("CrossCodex Test"), pkigen.WithDNSNames(graphITReviewerCN),
		pkigen.WithExtKeyUsage(x509.ExtKeyUsageClientAuth))
	Expect(err).NotTo(HaveOccurred(), "issue reviewer certificate")
	Expect(leaf.Cert.Subject.CommonName).To(Equal(graphITReviewerCN))

	dir := GinkgoT().TempDir()
	certPath, keyPath := filepath.Join(dir, "reviewer.pem"), filepath.Join(dir, "reviewer-key.pem")
	Expect(os.WriteFile(certPath, leaf.CertPEM, 0o644)).To(Succeed())
	Expect(os.WriteFile(keyPath, leaf.KeyPEM, 0o600)).To(Succeed())
	return config.TLSConfig{Mode: "mutual", CA: testTLSCAPath, Cert: certPath, Key: keyPath}
}

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go.
var _ = Describe("crosscodexd role=graph on a split stack", func() {
	It("serves graph, crosswalk, cleaning and feedback RPCs to the gateway, records the reviewer, and refuses other certificates", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		ctx := context.Background()
		natsURL := startSharedTestNATS(GinkgoT())
		tenantID := fmt.Sprintf("split-graph-%s", uuid.New().String())
		fakeLLM := testspecs.DeterministicLLM{EmbeddingDim: e2eEmbeddingDim}

		pipelineCfg := splitRoleConfig(dsn, natsURL, tenantID, config.RolePipeline)
		pipelineCfg.Pipeline.Addr = "127.0.0.1:0"
		pipelineCfg.Health.Addr = "127.0.0.1:0"
		pipelineCfg.TLS = testServerTLS()
		// Every role refuses a default tenant migrate has not provisioned.
		Expect(migrateDatabase(ctx, pipelineCfg, GinkgoWriter)).To(Succeed(), "provision the default tenant")

		pipelineRT, err := bootstrap(ctx, pipelineCfg, WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap pipeline role")
		defer pipelineRT.close()
		Expect(pipelineRT.start(ctx)).To(Succeed(), "start pipeline role")
		defer func() { _ = pipelineRT.stop(context.Background()) }()

		workerRT, err := bootstrap(ctx, splitRoleConfig(dsn, natsURL, tenantID, config.RoleWorker), WithLLMClient(fakeLLM))
		Expect(err).NotTo(HaveOccurred(), "bootstrap worker role")
		defer workerRT.close()
		Expect(workerRT.start(ctx)).To(Succeed(), "start worker role")
		defer func() { _ = workerRT.stop(context.Background()) }()

		// The graph role reads current stage records from the pipeline role
		// for cleaning, and signs decisions, so it needs both wired.
		graphRT := startGraphRole(ctx, dsn, natsURL, func(cfg *config.Config) {
			cfg.Pipeline.Endpoint = pipelineRT.pipelineServer.Addr()
			cfg.Tenants.DefaultTenant = tenantID
			cfg.Attestation = pipelineCfg.Attestation
			report := config.SignalPolicy{Action: config.CleaningActionReport}
			lowAgreement := report
			lowAgreement.MinAgreementFraction = 0.5
			cfg.Cleaning = config.CleaningConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
				EquivalenceFanout: report, CrossJobContradiction: report, LowAgreement: lowAgreement,
				ConflictsWith: report, StalePrompt: report, StaleModel: report,
			}}
		})
		Expect(graphRT.graphServer).NotTo(BeNil(), "graph role must own a standalone listener")
		graphAddr := graphRT.graphServer.Addr()

		reviewerTLS := issueReviewerTLS()

		gwCfg := baseTestConfig(dsn)
		gwCfg.Role = config.RoleGateway
		gwCfg.Server.Addr = "127.0.0.1:0"
		gwCfg.Pipeline.Endpoint = pipelineRT.pipelineServer.Addr()
		gwCfg.Graph.Endpoint = graphAddr
		gwCfg.TLS = testClientTLS()
		// An archive backend, but no database.purge_dsn: archiving works, so
		// the purge itself is what the last step shows refused.
		gwCfg.Retention.Archive.Backend = "local"
		archiveDir := GinkgoT().TempDir()
		gwCfg.Retention.Archive.Local.Path = archiveDir
		gwCfg.TLS.Targets = map[string]config.TLSOverride{
			"gateway-server": {Cert: testTLSServerCertPath, Key: testTLSServerKeyPath},
		}
		gwCfg.Auth.X509Mappings = append(slices.Clone(testClientX509Mappings), config.X509MappingConfig{
			Match: config.X509MatchConfig{CN: graphITReviewerCN, Organization: "CrossCodex Test"},
			Roles: []string{authn.RoleReviewer},
		})
		gwCfg.NATS = config.NATSConfig{URL: natsURL, PublishAckTimeout: 10 * time.Second}
		gwCfg.Tenants.DefaultTenant = tenantID
		gwCfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		gwRT, err := bootstrap(ctx, gwCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role")
		defer gwRT.close()
		Expect(gwRT.start(ctx)).To(Succeed(), "start gateway role")
		defer func() { _ = gwRT.stop(context.Background()) }()
		Expect(gwRT.shared.graphDB_).To(BeNil(), "the gateway role must hold no graph credentials")

		httpClientFor := func(tlsCfg config.TLSConfig, target string) *http.Client {
			built, err := tlsconfig.BuildTLSConfig(ctx, tlsCfg, target)
			Expect(err).NotTo(HaveOccurred(), "build %s TLS config", target)
			return &http.Client{Transport: &http.Transport{TLSClientConfig: built}}
		}
		gwURL := "https://" + gwRT.gatewayServer.Addr()
		adminHTTP := httpClientFor(testClientTLS(), "gateway-client")
		reviewerHTTP := httpClientFor(reviewerTLS, "gateway-client")
		admin := crosscodexv1connect.NewGatewayServiceClient(adminHTTP, gwURL)
		adminCleaning := crosscodexv1connect.NewEdgeCleaningServiceClient(adminHTTP, gwURL)
		reviewerCleaning := crosscodexv1connect.NewEdgeCleaningServiceClient(reviewerHTTP, gwURL)
		reviewer := crosscodexv1connect.NewGatewayServiceClient(reviewerHTTP, gwURL)
		adminService := crosscodexv1connect.NewAdminServiceClient(adminHTTP, gwURL)
		tenantCtx := &pb.TenantContext{TenantId: tenantID}

		By("importing a catalog and completing a job through the gateway")
		doc, err := os.ReadFile(e2eCatalogFixture)
		Expect(err).NotTo(HaveOccurred(), "read e2e catalog fixture")
		submitResp, err := admin.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_Content{Content: doc},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "split-graph",
		}))
		Expect(err).NotTo(HaveOccurred(), "SubmitDocument")
		catalogID := submitResp.Msg.GetCatalogId()
		Expect(catalogID).NotTo(BeEmpty(), "catalog id")
		startResp, err := admin.StartAnalysis(ctx, connect.NewRequest(&pb.StartAnalysisRequest{SourceCatalogId: catalogID, TargetCatalogId: catalogID}))
		Expect(err).NotTo(HaveOccurred(), "StartAnalysis")
		jobID := startResp.Msg.GetJobId()
		Expect(jobID).NotTo(BeEmpty(), "job id")
		Eventually(func(inner Gomega) {
			getResp, err := admin.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			inner.Expect(err).NotTo(HaveOccurred(), "GetJob")
			job := getResp.Msg.GetJob()
			if job.GetStatus() == pb.JobStatus_JOB_STATUS_FAILED {
				StopTrying(fmt.Sprintf("job %s failed: %s", jobID, job.GetError().GetMessage())).Now()
			}
			inner.Expect(job.GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), "job status")
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "job completes")

		By("answering QueryGraph, FindSimilar and GetCrosswalk through the gateway")
		// The graph role projects the catalog's three Control nodes
		// asynchronously; the tenant is unique to this spec, so the count is
		// exact.
		Eventually(func(inner Gomega) {
			resp, err := admin.QueryGraph(ctx, connect.NewRequest(&pb.QueryGraphRequest{Cypher: "MATCH (n:Control) RETURN n"}))
			inner.Expect(err).NotTo(HaveOccurred(), "QueryGraph")
			inner.Expect(resp.Msg.GetResponse().GetRowCount()).To(BeEquivalentTo(3), "Control nodes")
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "QueryGraph returns the projected controls")

		// The job embeds the two requirement items, whose statements are
		// identical, so each is the other's match.
		similar, err := admin.FindSimilar(ctx, connect.NewRequest(&pb.FindSimilarRequest{ControlId: catalogID + "/ac-2_smt.a"}))
		Expect(err).NotTo(HaveOccurred(), "FindSimilar")
		var matched []string
		for _, m := range similar.Msg.GetResponse().GetMatches() {
			matched = append(matched, m.GetNode().GetNodeId())
		}
		Expect(matched).To(ContainElement(catalogID+"/ac-2_smt.b"), "FindSimilar matches")

		crosswalk, err := admin.GetCrosswalk(ctx, connect.NewRequest(&pb.GetCrosswalkRequest{
			Mode: &pb.GetCrosswalkRequest_JobId{JobId: jobID},
		}))
		Expect(err).NotTo(HaveOccurred(), "GetCrosswalk")
		Expect(crosswalk.Msg.GetProvenance().GetMode()).To(Equal("job"))
		Expect(crosswalk.Msg.GetProvenance().GetSelectedJobIds()).To(ConsistOf(jobID))

		By("serving the review queue and determinations through the gateway from the graph role")
		_, err = reviewer.GetReviewQueue(ctx, connect.NewRequest(&pb.GetReviewQueueRequest{JobId: jobID}))
		Expect(err).NotTo(HaveOccurred(), "GetReviewQueue as the reviewer")
		// The graph role's feedback service refuses a determination
		// Unavailable, before reading the job, unless it was built with
		// WithDeterminations; NotFound proves it read the job's mappings.
		_, err = reviewer.RecordDetermination(ctx, connect.NewRequest(&pb.RecordDeterminationRequest{
			JobId: jobID, MappingId: "not-a-mapping-of-the-job", DeterminationRationale: "checked",
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound), "RecordDetermination: %v", err)

		By("serving AdminService on the gateway")
		// No database.purge_dsn here: stats, holds and dry runs still work.
		stats, err := adminService.GetRetentionStats(ctx, connect.NewRequest(&pb.GetRetentionStatsRequest{TenantContext: tenantCtx}))
		Expect(err).NotTo(HaveOccurred(), "GetRetentionStats: Unimplemented means attachGateway mounts no AdminService")
		Expect(stats.Msg.GetEligible()).NotTo(BeNil())
		scan, err := adminService.ScanRetention(ctx, connect.NewRequest(&pb.ScanRetentionRequest{TenantContext: tenantCtx, DryRun: true}))
		Expect(err).NotTo(HaveOccurred(), "ScanRetention dry run")
		Expect(scan.Msg.GetReport()).NotTo(BeNil())

		By("previewing a cleaning pass through the gateway")
		preview, err := adminCleaning.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
			TenantContext: tenantCtx, Mode: pb.CleaningMode_CLEANING_MODE_PREVIEW,
		}))
		Expect(err).NotTo(HaveOccurred(), "RunCleaning preview")
		Expect(preview.Msg.GetPlanToken()).NotTo(BeEmpty(), "plan token")

		By("seeding a reported finding and registering the reviewer")
		// The deterministic LLM raises no signal, so the finding is written
		// through the real store as app_user; the reviewer registry is
		// written through the superuser, the privileged path.
		suPool, err := db.NewPool(db.PoolConfig{DSN: dsn, MaxOpenConns: 2, Extensions: []string{"age", "vector"}})
		Expect(err).NotTo(HaveOccurred())
		defer suPool.Close()
		Expect(suPool.Exec(ctx, `INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ($1, $2, 'graph-role-it')`,
			tenantID, graphITReviewerCN)).To(Succeed(), "register the reviewer")
		finding := cleaningtest.ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusReported, catalogID, catalogID)
		ctxTenant, err := tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cleaning.NewPGFindingStore(db.NewTenantPool(graphRT.shared.appPool)).ApplyRun(ctxTenant, tenantID, cleaning.RunWrite{
			Run: cleaningtest.ContractRun(1), Upserts: cleaningtest.NewUpserts(finding),
		})).To(Succeed(), "seed the finding")

		read, err := reviewerCleaning.GetFinding(ctx, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tenantCtx, FindingId: finding.ID}))
		Expect(err).NotTo(HaveOccurred(), "GetFinding as the reviewer")
		stateToken := read.Msg.GetStateToken()
		Expect(stateToken).NotTo(BeEmpty())
		decisionCount := func() int {
			var n int
			Expect(suPool.QueryRow(ctx, `SELECT count(*) FROM edge_finding_decisions WHERE tenant_id = $1`, tenantID).Scan(&n)).To(Succeed())
			return n
		}

		By("refusing a CA-signed certificate outside tls.service_identities that dials the graph listener directly")
		// The reviewer bypasses the gateway and asserts itself as the end
		// user; only the gateway's certificate may assert a tenant or user.
		direct := crosscodexv1connect.NewEdgeCleaningServiceClient(httpClientFor(reviewerTLS, "graph-client"), "https://"+graphAddr)
		bypass := connect.NewRequest(&pb.DecideFindingRequest{
			TenantContext: tenantCtx, FindingId: finding.ID, Verdict: cleaning.VerdictDismiss,
			Rationale: "bypassing the gateway", ExpectStateToken: stateToken,
		})
		bypass.Header().Set(connectutil.MetadataKeyTenantID, tenantID)
		bypass.Header().Set(connectutil.MetadataKeyUserID, graphITReviewerCN)
		bypass.Header().Set(connectutil.MetadataKeyUserRoles, authn.RoleReviewer)
		_, err = direct.DecideFinding(ctx, bypass)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied), "direct dial: %v", err)
		Expect(err).To(MatchError(ContainSubstring("the client certificate CN=" + graphITReviewerCN + " is not listed in tls.service_identities")))
		Expect(err).To(MatchError(ContainSubstring("the graph role calling the pipeline role")))
		Expect(decisionCount()).To(BeZero(), "the refused decision recorded nothing")
		reread, err := reviewerCleaning.GetFinding(ctx, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tenantCtx, FindingId: finding.ID}))
		Expect(err).NotTo(HaveOccurred())
		Expect(reread.Msg.GetFinding().GetStatus()).To(Equal(string(cleaning.StatusReported)))
		Expect(reread.Msg.GetStateToken()).To(Equal(stateToken), "the finding is unchanged")

		By("recording the reviewer's certificate subject as decided_by")
		decided, err := reviewerCleaning.DecideFinding(ctx, connect.NewRequest(&pb.DecideFindingRequest{
			TenantContext: tenantCtx, FindingId: finding.ID, Verdict: cleaning.VerdictDismiss,
			Rationale: "the low agreement is expected for this pair", ExpectStateToken: stateToken,
		}))
		Expect(err).NotTo(HaveOccurred(), "DecideFinding as the reviewer")
		Expect(decided.Msg.GetFinding().GetStatus()).To(Equal(string(cleaning.StatusDismissed)))
		var decidedBy string
		Expect(suPool.QueryRow(ctx, `SELECT decided_by FROM edge_finding_decisions WHERE tenant_id = $1 AND decision_id = $2`,
			tenantID, decided.Msg.GetDecisionId()).Scan(&decidedBy)).To(Succeed(), "read the stored decision")
		Expect(decidedBy).To(Equal(graphITReviewerCN))

		By("refusing to purge an expired job up front on the gateway role, which has no database.purge_dsn, and keeping it")
		// Last, because a seeded completed job without stages would be read by
		// the cleaning steps above. jobs' RLS also matches created_by to the
		// caller, so the job is created by whoever created the one submitted above.
		var jobCreator string
		Expect(suPool.QueryRow(ctx, `SELECT created_by FROM jobs WHERE tenant_id = $1 LIMIT 1`, tenantID).Scan(&jobCreator)).To(Succeed())
		expiredJob := "expired-" + uuid.New().String()
		Expect(suPool.Exec(ctx, `INSERT INTO jobs (job_id, tenant_id, status, created_by, created_at)
			VALUES ($1, $2, 'completed', $3, now() - interval '400 days')`, expiredJob, tenantID, jobCreator)).To(Succeed())
		jobRows := func() int {
			var n int
			Expect(suPool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE tenant_id = $1 AND job_id = $2`, tenantID, expiredJob).Scan(&n)).To(Succeed())
			return n
		}
		expiredPolicy := &pb.RetentionPolicy{JobResults: "24h", Catalogs: "24h", Attestation: "24h"}
		// A dry run first proves the job is eligible, so the refusal below is
		// not just an empty scan.
		retentionPreview, err := adminService.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
			TenantContext: tenantCtx, Policy: expiredPolicy, DryRun: true}))
		Expect(err).NotTo(HaveOccurred(), "ApplyRetentionPolicy dry run")
		Expect(retentionPreview.Msg.GetReport().GetScanned()).To(BeEquivalentTo(1), "only the seeded job is expired: %v", retentionPreview.Msg.GetReport())
		_, err = adminService.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
			TenantContext: tenantCtx, Policy: expiredPolicy, DryRun: false}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition), "ApplyRetentionPolicy: %v", err)
		Expect(err).To(MatchError(ContainSubstring("database.purge_dsn is unset")))
		Expect(jobRows()).To(Equal(1), "the refused purge kept the job")
		// The archive provider creates its own root directory at startup, so
		// look for archived files rather than for an empty directory.
		var archived []string
		Expect(filepath.WalkDir(archiveDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type().IsRegular() {
				archived = append(archived, path)
			}
			return nil
		})).To(Succeed(), "walk the archive directory")
		Expect(archived).To(BeEmpty(), "the refusal came before archiving, so no orphaned archive copy exists")

		By("purging the job on a second gateway role with database.purge_dsn set to purge_user, then closing its purge pool")
		purgePassword, err := dbtest.RolePassword(dbtest.PurgeUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "purge_user password")
		purgeCfg := *gwCfg
		purgeCfg.Database.PurgeDSN, err = dbtest.RoleDSN(dsn, "purge_user", purgePassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(migrateDatabase(ctx, &purgeCfg, GinkgoWriter)).To(Succeed(), "set purge_user's password")
		purgeGwRT, err := bootstrap(ctx, &purgeCfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap gateway role with purge_dsn")
		defer purgeGwRT.close()
		Expect(purgeGwRT.start(ctx)).To(Succeed(), "start gateway role with purge_dsn")
		defer func() { _ = purgeGwRT.stop(context.Background()) }()
		Expect(purgeGwRT.retentionPurgePool).NotTo(BeNil(), "the gateway role builds the dedicated purge pool")

		purgeAdmin := crosscodexv1connect.NewAdminServiceClient(adminHTTP, "https://"+purgeGwRT.gatewayServer.Addr())
		purge, err := purgeAdmin.ApplyRetentionPolicy(ctx, connect.NewRequest(&pb.ApplyRetentionPolicyRequest{
			TenantContext: tenantCtx, Policy: expiredPolicy, DryRun: false}))
		Expect(err).NotTo(HaveOccurred(), "ApplyRetentionPolicy with purge_dsn")
		Expect(purge.Msg.GetReport().GetErrors()).To(BeEmpty())
		Expect(purge.Msg.GetReport().GetPurged()).To(BeEquivalentTo(1), "the expired job was purged: %v", purge.Msg.GetReport())
		Expect(jobRows()).To(BeZero(), "purge_user deleted the job")

		Expect(purgeGwRT.stop(context.Background())).To(Succeed())
		purgeGwRT.close()
		Expect(purgeGwRT.retentionPurgePool.Exec(ctx, "SELECT 1")).NotTo(Succeed(), "close released the purge pool")
	})
})
