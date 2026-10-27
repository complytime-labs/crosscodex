//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// equivalentRelationshipLLMClient answers the relationship prompt with
// EQUIVALENT so the analyzer and synthesis stages produce mappings to read
// back. Every other prompt keeps testspecs.DeterministicLLM's answers.
type equivalentRelationshipLLMClient struct {
	testspecs.DeterministicLLM
}

func (c equivalentRelationshipLLMClient) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	resp, err := c.DeterministicLLM.Complete(ctx, req)
	if err != nil || req.PromptName != "relationship" {
		return resp, err
	}
	resp.Choices[0].Message.Content = "RELATIONSHIP: EQUIVALENT\nJUSTIFICATION: same control intent\nCONFIDENCE: HIGH"
	return resp, nil
}

// exportedMapping is the subset of an exported ControlMapping the spec
// checks. The export encodes each mapping as proto JSON: lowerCamelCase field
// names and enum names, with unset fields such as a false isViable present.
type exportedMapping struct {
	MappingID        string `json:"mappingId"`
	SourceControlID  string `json:"sourceControlId"`
	TargetControlID  string `json:"targetControlId"`
	RelationshipType string `json:"relationshipType"`
	IsViable         bool   `json:"isViable"`
}

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go.
var _ = Describe("crosscodex results: mappings read back from a completed all-role job", Ordered, func() {
	var (
		cliPath  string
		cliHome  string
		endpoint string
		jobID    string
		g        graphdb.GraphDB
		tenantID string
	)

	// runCLI runs the built crosscodex binary against the daemon over mTLS.
	// The environment is rebuilt from scratch with a temporary HOME and XDG
	// directories, so the developer's own CLI config, profiles and endpoint
	// variables are never read.
	runCLI := func(args ...string) (string, string, error) {
		full := append([]string{
			"--endpoint", endpoint,
			"--tls-ca", testTLSCAPath,
			"--tls-cert", testTLSClientCertPath,
			"--tls-key", testTLSClientKeyPath,
		}, args...)
		cmd := exec.Command(cliPath, full...)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + cliHome,
			"XDG_CONFIG_HOME=" + filepath.Join(cliHome, "config"),
			"XDG_STATE_HOME=" + filepath.Join(cliHome, "state"),
			"XDG_DATA_HOME=" + filepath.Join(cliHome, "data"),
			"NO_COLOR=1",
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	exportMappings := func(args ...string) []exportedMapping {
		stdout, stderr, err := runCLI(append([]string{"results", "export", jobID, "--format", "json"}, args...)...)
		Expect(err).NotTo(HaveOccurred(), "results export %v (stderr: %s)", args, stderr)
		var out struct {
			JobID    string            `json:"job_id"`
			Mappings []exportedMapping `json:"mappings"`
		}
		Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed(), "decode export JSON: %s", stdout)
		Expect(out.JobID).To(Equal(jobID), "export job_id")
		return out.Mappings
	}

	BeforeAll(func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		ctx := context.Background()

		// cmd/crosscodex and cmd/crosscodexd are both package main, so the
		// CLI cannot be linked into this test binary; build it instead.
		gomod, err := exec.Command("go", "env", "GOMOD").Output()
		Expect(err).NotTo(HaveOccurred(), "go env GOMOD")
		repoRoot := filepath.Dir(strings.TrimSpace(string(gomod)))
		binDir := GinkgoT().TempDir()
		cliPath = filepath.Join(binDir, "crosscodex")
		build := exec.Command("go", "build", "-o", cliPath, "./cmd/crosscodex")
		build.Dir = repoRoot
		buildOut, err := build.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "build crosscodex CLI: %s", buildOut)
		cliHome = GinkgoT().TempDir()

		tenantID = fmt.Sprintf("results-e2e-%s", uuid.New().String())
		cfg := allRoleTestConfig(dsn, tenantID)
		Expect(migrateDatabase(ctx, cfg, GinkgoWriter)).To(Succeed(), "provision the default tenant")

		rt, err := bootstrap(ctx, cfg, WithLLMClient(equivalentRelationshipLLMClient{DeterministicLLM: testspecs.DeterministicLLM{EmbeddingDim: e2eEmbeddingDim}}))
		Expect(err).NotTo(HaveOccurred(), "bootstrap all role")
		DeferCleanup(rt.close)
		Expect(rt.start(ctx)).To(Succeed(), "start all role")
		DeferCleanup(func() { _ = rt.stop(context.Background()) })

		endpoint = "https://" + rt.gatewayServer.Addr()
		g = rt.shared.graphDB_

		clientTLS, err := tlsconfig.BuildTLSConfig(ctx, testClientTLS(), "gateway-client")
		Expect(err).NotTo(HaveOccurred(), "build client TLS config")
		httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}
		client := crosscodexv1connect.NewGatewayServiceClient(httpClient, endpoint)

		doc, err := os.ReadFile(e2eCatalogFixture)
		Expect(err).NotTo(HaveOccurred(), "read e2e catalog fixture")
		submitResp, err := client.SubmitDocument(ctx, connect.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_Content{Content: doc},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "e2e-minimal",
		}))
		Expect(err).NotTo(HaveOccurred(), "SubmitDocument")
		jobID = submitResp.Msg.GetJobId()
		Expect(jobID).NotTo(BeEmpty(), "job id")

		Eventually(func() pb.JobStatus {
			getResp, err := client.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
			if err != nil {
				return pb.JobStatus_JOB_STATUS_UNSPECIFIED
			}
			return getResp.Msg.GetJob().GetStatus()
		}, 90*time.Second, 500*time.Millisecond).Should(
			Or(Equal(pb.JobStatus_JOB_STATUS_COMPLETED), Equal(pb.JobStatus_JOB_STATUS_FAILED)),
		)
		getResp, err := client.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{JobId: jobID}))
		Expect(err).NotTo(HaveOccurred(), "GetJob")
		Expect(getResp.Msg.GetJob().GetStatus()).To(Equal(pb.JobStatus_JOB_STATUS_COMPLETED),
			fmt.Sprintf("job should complete (error: %s)", getResp.Msg.GetJob().GetError().GetMessage()))
	})

	It("exports the analyzer stage's EQUIVALENT votes, none of them viable", func() {
		mappings := exportMappings("--stage", "analyzer")
		Expect(mappings).NotTo(BeEmpty(), "analyzer mappings")
		for _, m := range mappings {
			Expect(m.RelationshipType).To(Equal(pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT.String()),
				"analyzer mapping %s relationship type", m.MappingID)
			Expect(m.IsViable).To(BeFalse(), "analyzer mapping %s carries no viability", m.MappingID)
		}
	})

	It("exports viable synthesized mappings whose IDs are the SEMANTIC_MATCH edge IDs", func() {
		mappings := exportMappings()
		Expect(mappings).To(ContainElement(HaveField("IsViable", BeTrue())), "a viable synthesized mapping")

		// The graph subscriber materializes SEMANTIC_MATCH edges after the job
		// completes; poll until every exported pair's edge has landed.
		Eventually(func(inner Gomega) {
			edges, err := g.QueryRelationships(context.Background(), tenantID,
				graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
			inner.Expect(err).NotTo(HaveOccurred(), "query SEMANTIC_MATCH")
			edgeIDs := make(map[[2]string]string, len(edges))
			for _, e := range edges {
				edgeIDs[[2]string{e.Source.ID, e.Target.ID}] = e.Edge.ID
			}
			for _, m := range mappings {
				pair := [2]string{m.SourceControlID, m.TargetControlID}
				inner.Expect(edgeIDs).To(HaveKeyWithValue(pair, m.MappingID),
					"SEMANTIC_MATCH edge for %s -> %s", m.SourceControlID, m.TargetControlID)
			}
		}, 60*time.Second, 500*time.Millisecond).Should(Succeed(), "mapping IDs match graph edge IDs")
	})

	It("summarizes the same mappings the export returns", func() {
		exported := exportMappings()
		stdout, stderr, err := runCLI("--json", "results", "summary", jobID)
		Expect(err).NotTo(HaveOccurred(), "results summary (stderr: %s)", stderr)
		var summary struct {
			JobID  string         `json:"job_id"`
			Total  int            `json:"total"`
			ByType map[string]int `json:"by_type"`
		}
		Expect(json.Unmarshal([]byte(stdout), &summary)).To(Succeed(), "decode summary JSON: %s", stdout)
		Expect(summary.JobID).To(Equal(jobID), "summary job_id")
		Expect(summary.Total).To(Equal(len(exported)), "summary total equals export count")
		Expect(summary.ByType).To(HaveKeyWithValue("RELATIONSHIP_TYPE_EQUIVALENT", len(exported)),
			"summary by_type")
	})

	It("rejects a summary for a job that does not exist", func() {
		_, stderr, err := runCLI("results", "summary", uuid.New().String())
		Expect(err).To(HaveOccurred(), "summary of an unknown job exits non-zero")
		Expect(stderr).To(ContainSubstring("not found"), "summary of an unknown job explains why")
	})
})
