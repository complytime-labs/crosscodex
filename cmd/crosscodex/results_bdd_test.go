package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
	var (
		stdout bytes.Buffer
		fake   *fakeMappingsClient
	)

	run := func(args ...string) error {
		stdout.Reset()
		cmd := newResultsCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}

	BeforeEach(func() {
		fake = &fakeMappingsClient{pages: []*pb.ListJobMappingsResponse{mappingsPage("")}}
	})

	DescribeTable("sends the job id and server defaults",
		func(sub string) {
			Expect(run(sub, "job-1")).To(Succeed())
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
			Expect(run(sub, "job-1", "--stage", "synthesized", "--min-confidence", "0.5",
				"--type", "equivalent", "--type", "SUBSET_OF", "--control", "AC-1", "--include-non-viable")).To(Succeed())
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
		Expect(run("summary", "job-1", "--stage", "analyzer")).To(Succeed())
		Expect(fake.calls[0].GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_ANALYZER))
	})

	DescribeTable("rejects invalid flags with an actionable error and never calls the server",
		func(sub string, want string, args ...string) {
			err := run(append([]string{sub, "job-1"}, args...)...)
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
		err := run("export", "job-1")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`list mappings for job job-1: server returned page token "tok" again`))
		Expect(err.Error()).To(ContainSubstring("crosscodexd bug"))
		Expect(fake.calls).To(HaveLen(2))
	})

	It("reports the canonical stage name for mixed-case --stage", func() {
		Expect(run("summary", "job-1", "--stage", "Analyzer")).To(Succeed())
		Expect(stdout.String()).To(ContainSubstring("(analyzer mappings)"))
		Expect(stdout.String()).NotTo(ContainSubstring("Analyzer"))

		fake.pages = append(fake.pages, mappingsPage(""))
		Expect(run("--json", "summary", "job-1", "--stage", "ANALYZER")).To(Succeed())
		Expect(stdout.String()).To(ContainSubstring(`"stage": "analyzer"`))
	})

	It("exports JSON with enum names and unset fields present", func() {
		nonViable := &pb.ControlMapping{MappingId: "n", SourceControlId: "s-n", TargetControlId: "t-n", RelationshipType: pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF}
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT), nonViable),
		}
		Expect(run("export", "job-1", "--format", "json")).To(Succeed())

		var out struct {
			JobID    string           `json:"job_id"`
			Mappings []map[string]any `json:"mappings"`
		}
		Expect(json.Unmarshal(stdout.Bytes(), &out)).To(Succeed(), "export output is valid JSON: %s", stdout.String())
		Expect(out.JobID).To(Equal("job-1"))
		Expect(out.Mappings).To(HaveLen(2))
		Expect(out.Mappings[0]).To(HaveKeyWithValue("relationshipType", "RELATIONSHIP_TYPE_EQUIVALENT"))
		Expect(out.Mappings[0]).To(HaveKeyWithValue("isViable", true))
		Expect(out.Mappings[1]).To(HaveKeyWithValue("relationshipType", "RELATIONSHIP_TYPE_SUBSET_OF"))
		Expect(out.Mappings[1]).To(HaveKeyWithValue("isViable", false), "a false is_viable is exported, not omitted")
		Expect(out.Mappings[1]).To(HaveKeyWithValue("confidence", BeNumerically("==", 0)), "zero values are exported")
	})

	It("exports an empty mapping list as an empty JSON array", func() {
		Expect(run("export", "job-1", "--format", "json")).To(Succeed())
		Expect(stdout.String()).To(MatchJSON(`{"job_id": "job-1", "mappings": []}`))
	})

	It("pages until the server returns no next token", func() {
		fake.pages = []*pb.ListJobMappingsResponse{
			mappingsPage("tok-2", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)),
			mappingsPage("", mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF)),
		}
		Expect(run("export", "job-1", "--format", "csv")).To(Succeed())
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetPagination().GetPageToken()).To(BeEmpty())
		Expect(fake.calls[1].GetPagination().GetPageToken()).To(Equal("tok-2"))
		lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
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
		Expect(run("summary", "job-1")).To(Succeed())
		out := stdout.String()
		Expect(out).To(ContainSubstring("Total mappings: 3"))
		eq := strings.Index(out, "RELATIONSHIP_TYPE_EQUIVALENT")
		sub := strings.Index(out, "RELATIONSHIP_TYPE_SUBSET_OF")
		Expect(eq).To(BeNumerically(">=", 0))
		Expect(eq).To(BeNumerically("<", sub))
	})

	It("keeps the summary JSON shape and adds the stage", func() {
		fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT))}
		Expect(run("--json", "summary", "job-1")).To(Succeed())
		Expect(stdout.String()).To(And(
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
			err := run(args...)
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
		Expect(run("debug", "job-1", "ctrl-1")).To(Succeed())
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetJobId()).To(Equal("job-1"))
		Expect(fake.calls[0].GetControlId()).To(Equal("ctrl-1"))
		Expect(fake.calls[0].GetStage()).To(Equal(pb.MappingStage_MAPPING_STAGE_SYNTHESIZED))
		Expect(stdout.String()).To(And(
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
		err := run("summary", "job-1")
		Expect(err).To(MatchError(ContainSubstring("again; stopping to avoid an endless loop")))
		Expect(fake.calls).To(HaveLen(3))
	})

	Describe("export validation", func() {
		It("rejects an unknown format before fetching or touching the output file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "out.json")
			Expect(os.WriteFile(path, []byte("precious"), 0o600)).To(Succeed())
			err := run("export", "job-1", "--format", "xml", "--output", path)
			Expect(err).To(MatchError(And(ContainSubstring(`"xml"`), ContainSubstring("json"), ContainSubstring("csv"))))
			Expect(fake.calls).To(BeEmpty())
			got, readErr := os.ReadFile(path)
			Expect(readErr).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal("precious"))
		})
	})
})

var _ = Describe("Results Commands", func() {
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	Describe("results summary", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "summary"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "summary"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("results summary"))
		})

		It("supports 'sum' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "sum"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("summary"))
		})
	})

	Describe("results export", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "export"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("has --format flag", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "export", "--help"})
			err := cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("--format"))
		})

		It("supports 'dump' alias", func() {
			cmd := newRootCmd()
			resultsCmd, _, err := cmd.Find([]string{"results", "dump"})
			Expect(err).NotTo(HaveOccurred())
			Expect(resultsCmd.Name()).To(Equal("export"))
		})
	})

	Describe("results query", func() {
		It("requires a cypher query argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "query"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cypher"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "query"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("MATCH"))
		})
	})

	Describe("results debug", func() {
		It("requires job-id and control-id arguments", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "debug"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})

		It("requires control-id when only job-id provided", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "debug", "job123"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("control-id"))
		})
	})

	Describe("results review", func() {
		It("requires a job-id argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "review"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job-id"))
		})
	})

	Describe("results verify", func() {
		It("requires a bundle-file argument", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bundle"))
		})

		It("includes usage examples in error message", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("results verify"))
		})

		It("validates valid JSON bundle without public key", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte(`{"payloadType": "application/vnd.in-toto+json", "payload": "test"}`), 0o644)).To(Succeed())
			tmpFile.Close()

			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", tmpFile.Name()})
			err = cmd.Execute()
			Expect(err).NotTo(HaveOccurred())
			output := stdout.String()
			Expect(output).To(ContainSubstring("Structure valid"))
		})

		It("rejects invalid JSON bundle", func() {
			tmpFile, err := os.CreateTemp("", "test-bundle-*.json")
			Expect(err).NotTo(HaveOccurred())
			defer os.Remove(tmpFile.Name())
			Expect(os.WriteFile(tmpFile.Name(), []byte("not json"), 0o644)).To(Succeed())
			tmpFile.Close()

			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", tmpFile.Name()})
			err = cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not valid JSON"))
		})

		It("errors when bundle file does not exist", func() {
			cmd := newRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"results", "verify", "/nonexistent/bundle.json"})
			err := cmd.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("read bundle file"))
		})
	})
})
