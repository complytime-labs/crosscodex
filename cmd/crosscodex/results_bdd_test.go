package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeMappingsClient serves ListJobMappings from a queue of pages and
// records every request; other GatewayServiceClient methods panic through
// the nil embed.
type fakeMappingsClient struct {
	crosscodexv1connect.GatewayServiceClient
	pages []*pb.ListJobMappingsResponse
	err   error
	calls []*pb.ListJobMappingsRequest
}

func (f *fakeMappingsClient) ListJobMappings(_ context.Context, req *connectrpc.Request[pb.ListJobMappingsRequest]) (*connectrpc.Response[pb.ListJobMappingsResponse], error) {
	// Copy: the CLI mutates one request across pages.
	f.calls = append(f.calls, &pb.ListJobMappingsRequest{
		JobId:             req.Msg.GetJobId(),
		Stage:             req.Msg.GetStage(),
		MinConfidence:     req.Msg.GetMinConfidence(),
		IncludeNonViable:  req.Msg.GetIncludeNonViable(),
		RelationshipTypes: req.Msg.GetRelationshipTypes(),
		ControlId:         req.Msg.GetControlId(),
		Pagination:        &pb.Pagination{PageSize: req.Msg.GetPagination().GetPageSize(), PageToken: req.Msg.GetPagination().GetPageToken()},
	})
	if f.err != nil {
		return nil, f.err
	}
	page := f.pages[len(f.calls)-1]
	return connectrpc.NewResponse(page), nil
}

func mappingsPage(next string, ms ...*pb.ControlMapping) *pb.ListJobMappingsResponse {
	return &pb.ListJobMappingsResponse{Mappings: ms, PageInfo: &pb.PageInfo{NextPageToken: next}}
}

func mapping(id string, rt pb.RelationshipType) *pb.ControlMapping {
	return &pb.ControlMapping{MappingId: id, SourceControlId: "s-" + id, TargetControlId: "t-" + id, RelationshipType: rt, Confidence: 0.9, ViabilityScore: 0.8, IsViable: true}
}

var _ = Describe("Results mappings via ListJobMappings", func() {
	var fake *fakeMappingsClient

	newCmd := func() *cobra.Command {
		cmd := newResultsCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		return cmd
	}

	BeforeEach(func() {
		fake = &fakeMappingsClient{pages: []*pb.ListJobMappingsResponse{mappingsPage("")}}
	})

	DescribeTable("sends the job id and server defaults",
		func(sub string) {
			_, _, err := execute(newCmd(), sub, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(HaveLen(1))
			req := fake.calls[0]
			Expect(req.GetJobId()).To(Equal("job-1"))
			Expect(req.GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_SYNTHESIZED))
			Expect(req.GetMinConfidence()).To(BeZero())
			Expect(req.GetIncludeNonViable()).To(BeFalse())
			Expect(req.GetRelationshipTypes()).To(BeEmpty())
			Expect(req.GetControlId()).To(BeEmpty())
			Expect(req.GetPagination().GetPageSize()).To(Equal(int32(1000)))
			Expect(req.GetPagination().GetPageToken()).To(BeEmpty())
		},
		Entry("summary", "summary"),
		Entry("export", "export"),
	)

	DescribeTable("maps flags onto the request",
		func(sub string) {
			_, _, err := execute(newCmd(), sub, "job-1", "--stage", "synthesized", "--min-confidence", "0.5",
				"--type", "equivalent", "--type", "SUBSET_OF", "--control", "AC-1", "--include-non-viable")
			Expect(err).NotTo(HaveOccurred())
			req := fake.calls[0]
			Expect(req.GetMinConfidence()).To(BeNumerically("~", 0.5, 1e-6))
			Expect(req.GetRelationshipTypes()).To(Equal([]pb.RelationshipType{
				pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
				pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF,
			}))
			Expect(req.GetControlId()).To(Equal("AC-1"))
			Expect(req.GetIncludeNonViable()).To(BeTrue())
		},
		Entry("summary", "summary"),
		Entry("export", "export"),
	)

	It("selects the analyzer stage", func() {
		_, _, err := execute(newCmd(), "summary", "job-1", "--stage", "analyzer")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_ANALYZER))
	})

	DescribeTable("rejects invalid flags with an actionable error and never calls the server",
		func(sub string, want string, args ...string) {
			_, _, err := execute(newCmd(), append([]string{sub, "job-1"}, args...)...)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
			Expect(fake.calls).To(BeEmpty())
		},
		Entry("summary: unknown stage", "summary", "synthesized, analyzer", "--stage", "bogus"),
		Entry("summary: unknown type", "summary", "equivalent", "--type", "bogus"),
		Entry("summary: unspecified type", "summary", "equivalent", "--type", "unspecified"),
		Entry("summary: confidence above 1", "summary", "between 0 and 1", "--min-confidence", "1.5"),
		Entry("summary: negative confidence", "summary", "between 0 and 1", "--min-confidence", "-0.1"),
		Entry("summary: non-viable with analyzer stage", "summary", "synthesized", "--stage", "analyzer", "--include-non-viable"),
		Entry("export: unknown stage", "export", "synthesized, analyzer", "--stage", "bogus"),
		Entry("export: unknown type", "export", "equivalent", "--type", "bogus"),
		Entry("export: confidence above 1", "export", "between 0 and 1", "--min-confidence", "1.5"),
		Entry("export: non-viable with analyzer stage", "export", "synthesized", "--stage", "analyzer", "--include-non-viable"),
	)

	It("stops when the server repeats the page token it was given", func() {
		fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("tok"), mappingsPage("tok"), mappingsPage("tok")}
		_, _, err := execute(newCmd(), "export", "job-1")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`list mappings for job job-1: server returned page token "tok" again`))
		Expect(err.Error()).To(ContainSubstring("crosscodexd bug"))
		Expect(fake.calls).To(HaveLen(2))
	})

	It("reports the canonical stage name for mixed-case --stage", func() {
		stdout, _, err := execute(newCmd(), "summary", "job-1", "--stage", "Analyzer")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("(analyzer mappings)"))
		Expect(stdout).NotTo(ContainSubstring("Analyzer"))

		fake.pages = append(fake.pages, mappingsPage(""))
		stdout, _, err = execute(newCmd(), "--json", "summary", "job-1", "--stage", "ANALYZER")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring(`"stage": "analyzer"`))
	})

	It("exports JSON with enum names and unset fields present", func() {
		nonViable := &pb.ControlMapping{MappingId: "n", SourceControlId: "s-n", TargetControlId: "t-n", RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF}
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT), nonViable),
		}
		stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
		Expect(err).NotTo(HaveOccurred())

		var out struct {
			JobID    string           `json:"job_id"`
			Mappings []map[string]any `json:"mappings"`
		}
		Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed(), "export output is valid JSON: %s", stdout)
		Expect(out.JobID).To(Equal("job-1"))
		Expect(out.Mappings).To(HaveLen(2))
		Expect(out.Mappings[0]).To(HaveKeyWithValue("relationshipType", "RELATIONSHIP_TYPE_EQUIVALENT"))
		Expect(out.Mappings[0]).To(HaveKeyWithValue("isViable", true))
		Expect(out.Mappings[1]).To(HaveKeyWithValue("relationshipType", "RELATIONSHIP_TYPE_SUBSET_OF"))
		Expect(out.Mappings[1]).To(HaveKeyWithValue("isViable", false), "a false is_viable is exported, not omitted")
		Expect(out.Mappings[1]).To(HaveKeyWithValue("confidence", BeNumerically("==", 0)), "zero values are exported")
	})

	It("exports an empty mapping list as an empty JSON array", func() {
		stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(MatchJSON(`{"job_id": "job-1", "mappings": []}`))
	})

	It("pages until the server returns no next token", func() {
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("tok-2", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("", mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF)),
		}
		stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetPagination().GetPageToken()).To(BeEmpty())
		Expect(fake.calls[1].GetPagination().GetPageToken()).To(Equal("tok-2"))
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		Expect(lines).To(HaveLen(3))
		Expect(lines[0]).To(Equal("mapping_id,source_control_id,target_control_id,relationship_type,confidence,viability_score,is_viable"))
		Expect(lines[1]).To(HavePrefix("a,s-a,t-a,RELATIONSHIP_TYPE_EQUIVALENT,0.900,0.800,true"))
		Expect(lines[2]).To(HavePrefix("b,s-b,t-b,RELATIONSHIP_TYPE_SUBSET_OF"))
	})

	It("summarizes counts sorted by type name across pages", func() {
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("t", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF), mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("", mapping("c", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF)),
		}
		stdout, _, err := execute(newCmd(), "summary", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("Total mappings: 3"))
		eq := strings.Index(stdout, "RELATIONSHIP_TYPE_EQUIVALENT")
		sub := strings.Index(stdout, "RELATIONSHIP_TYPE_SUBSET_OF")
		Expect(eq).To(BeNumerically(">=", 0))
		Expect(eq).To(BeNumerically("<", sub))
	})

	It("keeps the summary JSON shape and adds the stage", func() {
		fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT))}
		stdout, _, err := execute(newCmd(), "--json", "summary", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(And(
			ContainSubstring(`"job_id": "job-1"`),
			ContainSubstring(`"total": 1`),
			ContainSubstring(`"mapping_count": 1`),
			ContainSubstring(`"by_type"`),
			ContainSubstring(`"stage": "synthesized"`),
		))
	})

	DescribeTable("surfaces server errors with the job id and the server message",
		func(args ...string) {
			fake.err = connectrpc.NewError(connectrpc.CodeNotFound, errors.New("job not found: check the id"))
			_, _, err := execute(newCmd(), args...)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("list mappings for job job-1"))
			Expect(err.Error()).To(ContainSubstring("job not found: check the id"))
		},
		Entry("summary", "summary", "job-1"),
		Entry("export", "export", "job-1"),
		Entry("debug", "debug", "job-1", "ctrl-1"),
	)

	It("debug reads the job's synthesized mappings of the control through ListJobMappings", func() {
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("t", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("", mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF)),
		}
		stdout, _, err := execute(newCmd(), "debug", "job-1", "ctrl-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetJobId()).To(Equal("job-1"))
		Expect(fake.calls[0].GetControlId()).To(Equal("ctrl-1"))
		Expect(fake.calls[0].GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_SYNTHESIZED))
		Expect(stdout).To(And(
			ContainSubstring("Debug for job job-1, control ctrl-1"),
			ContainSubstring("Found 2 mapping(s)"),
		))
	})

	It("stops when a page token repeats non-adjacently", func() {
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("A", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("B", mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("A", mapping("c", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("", mapping("d", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
		}
		_, _, err := execute(newCmd(), "summary", "job-1")
		Expect(err).To(MatchError(ContainSubstring("again; stopping to avoid an endless loop")))
		Expect(fake.calls).To(HaveLen(3))
	})

	Describe("export validation", func() {
		It("rejects an unknown format before fetching or touching the output file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "out.json")
			Expect(os.WriteFile(path, []byte("precious"), 0o600)).To(Succeed())
			_, _, err := execute(newCmd(), "export", "job-1", "--format", "xml", "--output", path)
			Expect(err).To(MatchError(And(ContainSubstring(`"xml"`), ContainSubstring("json"), ContainSubstring("csv"))))
			Expect(fake.calls).To(BeEmpty())
			got, readErr := os.ReadFile(path)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal("precious"))
		})
	})
})

var _ = Describe("Results Commands", func() {
	Describe("results summary", func() {
		It("supports 'sum' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "sum"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("summary"))
		})
	})

	Describe("results export", func() {
		It("has --format flag", func() {
			stdout, _, err := execute(newRootCmd(), "results", "export", "--help")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("--format"))
		})

		It("supports 'dump' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "dump"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("export"))
		})
	})

	DescribeTable("errors when a required argument is missing",
		func(args []string, wantSubstr string) {
			_, _, err := execute(newRootCmd(), args...)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantSubstr))
		},
		Entry("summary requires a job-id", []string{"results", "summary"}, "job-id"),
		Entry("summary error includes usage examples", []string{"results", "summary"}, "results summary"),
		Entry("export requires a job-id", []string{"results", "export"}, "job-id"),
		Entry("query requires a cypher query", []string{"results", "query"}, "cypher"),
		Entry("query error includes usage examples", []string{"results", "query"}, "MATCH"),
		Entry("debug requires a job-id and control-id", []string{"results", "debug"}, "job-id"),
		Entry("debug requires a control-id when only job-id is given", []string{"results", "debug", "job123"}, "control-id"),
		Entry("review requires a job-id", []string{"results", "review"}, "job-id"),
		Entry("verify requires a bundle-file", []string{"results", "verify"}, "bundle"),
		Entry("verify error includes usage examples", []string{"results", "verify"}, "results verify"),
	)

	Describe("results verify", func() {
		It("validates valid JSON bundle without public key", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte(`{"payloadType": "application/vnd.in-toto+json", "payload": "test"}`), 0o644)).To(Succeed())
			tmpFile.Close()

			stdout, _, err := execute(newRootCmd(), "results", "verify", tmpFile.Name())
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Structure valid"))
		})

		It("rejects invalid JSON bundle", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte("not json"), 0o644)).To(Succeed())
			tmpFile.Close()

			_, _, err = execute(newRootCmd(), "results", "verify", tmpFile.Name())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not valid JSON"))
		})

		It("errors when bundle file does not exist", func() {
			_, _, err := execute(newRootCmd(), "results", "verify", "/nonexistent/bundle.json")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("read bundle file"))
		})
	})
})
