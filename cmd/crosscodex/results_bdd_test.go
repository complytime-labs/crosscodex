package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
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

// CWE-1236: a spreadsheet runs a cell that starts with a formula character.
var _ = Describe("csvCell", func() {
	DescribeTable("prefixes a value a spreadsheet would run as a formula with a single quote",
		func(in, want string) {
			Expect(csvCell(in)).To(Equal(want))
		},
		Entry("equals sign", "=HYPERLINK(\"http://x\")", "'=HYPERLINK(\"http://x\")"),
		Entry("plus sign", "+1+1", "'+1+1"),
		Entry("minus sign", "-1+1", "'-1+1"),
		Entry("at sign", "@SUM(A1)", "'@SUM(A1)"),
		Entry("a control ID starting with a minus sign", "-AC-1", "'-AC-1"),
		Entry("a formula after leading whitespace, which cleaning trims", "\t=1+1", "'=1+1"),
	)

	It("leaves a benign value unchanged", func() {
		Expect(csvCell("AC-1 = Access Control")).To(Equal("AC-1 = Access Control"))
	})

	It("cleans a value to one line before checking it", func() {
		Expect(csvCell("Evil\x1b]0;pwned\x07\nCatalog")).To(Equal("Evil ]0;pwned Catalog"))
	})
})

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
		Expect(stdout).To(MatchJSON(`{"job_id": "job-1", "analysis": null, "mappings": []}`))
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
		Expect(lines[0]).To(Equal("mapping_id,source_control_id,target_control_id,relationship_type,confidence,viability_score,is_viable," +
			"job_source_catalog_id,job_source_catalog_name,job_source_catalog_version," +
			"job_target_catalog_id,job_target_catalog_name,job_target_catalog_version,job_bidirectional," +
			"job_source_catalog_snapshot,job_target_catalog_snapshot"))
		Expect(lines[1]).To(HavePrefix("a,s-a,t-a,RELATIONSHIP_TYPE_EQUIVALENT,0.900,0.800,true"))
		Expect(lines[2]).To(HavePrefix("b,s-b,t-b,RELATIONSHIP_TYPE_SUBSET_OF"))
	})

	Describe("export of the analyzer stage", func() {
		// The server sends analyzer mappings with zero viability: the stage
		// has none, so exporting 0.000/false would claim a verdict.
		BeforeEach(func() {
			m := mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT)
			m.ViabilityScore, m.IsViable = 0, false
			fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("", m)}
		})

		It("leaves the CSV viability cells empty", func() {
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv", "--stage", "analyzer")
			Expect(err).NotTo(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(HaveLen(2))
			Expect(lines[1]).To(HavePrefix("a,s-a,t-a,RELATIONSHIP_TYPE_EQUIVALENT,0.900,,,"))
		})

		It("omits the viability fields from JSON", func() {
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json", "--stage", "analyzer")
			Expect(err).NotTo(HaveOccurred())
			var out struct {
				Mappings []map[string]any `json:"mappings"`
			}
			Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed(), "export output is valid JSON: %s", stdout)
			Expect(out.Mappings).To(HaveLen(1))
			Expect(out.Mappings[0]).To(HaveKeyWithValue("confidence", BeNumerically("~", 0.9, 1e-6)))
			Expect(out.Mappings[0]).NotTo(HaveKey("viabilityScore"))
			Expect(out.Mappings[0]).NotTo(HaveKey("isViable"))
		})
	})

	Describe("export of a crosswalk job", func() {
		BeforeEach(func() {
			page := mappingsPage("", mapping("a", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT))
			page.Analysis = &pb.JobAnalysis{
				SourceCatalog: &pb.CatalogSummary{CatalogId: "cat-src", Name: "CSF 2.0", Version: "2.0", Imported: true, Snapshot: true},
				TargetCatalog: &pb.CatalogSummary{CatalogId: "cat-tgt", Name: "NIST SP 800-53", Imported: true, Snapshot: true},
			}
			fake.pages = []*pb.ListJobMappingsResponse{page}
		})

		// exportedAnalysis is the "analysis" object of a JSON export.
		type exportedAnalysis struct {
			Source        map[string]any `json:"sourceCatalog"`
			Target        map[string]any `json:"targetCatalog"`
			Bidirectional bool           `json:"bidirectional"`
		}

		It("puts the job's analysis in the JSON envelope, once", func() {
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
			Expect(err).NotTo(HaveOccurred())
			var out struct {
				Analysis exportedAnalysis `json:"analysis"`
				Mappings []map[string]any `json:"mappings"`
			}
			Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed())
			Expect(out.Analysis.Source).To(SatisfyAll(HaveKeyWithValue("catalogId", "cat-src"), HaveKeyWithValue("name", "CSF 2.0"), HaveKeyWithValue("version", "2.0"), HaveKeyWithValue("imported", true)))
			Expect(out.Analysis.Target).To(SatisfyAll(HaveKeyWithValue("catalogId", "cat-tgt"), HaveKeyWithValue("version", ""), HaveKeyWithValue("imported", true)))
			Expect(out.Analysis.Bidirectional).To(BeFalse())
			Expect(out.Mappings).To(HaveLen(1))
		})

		It("repeats the job's analysis on every CSV row", func() {
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
			Expect(err).NotTo(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(HaveLen(2))
			Expect(lines[1]).To(HaveSuffix(",cat-src,CSF 2.0,2.0,cat-tgt,NIST SP 800-53,,false,true,true"))
		})

		// A job created before snapshots reports its catalogs' current
		// labels; the export says which kind each side's label is.
		DescribeTable("says in CSV whether each side's label was recorded when the job was created",
			func(source, target bool, wantSuffix string) {
				fake.pages[0].Analysis.SourceCatalog.Snapshot = source
				fake.pages[0].Analysis.TargetCatalog.Snapshot = target
				stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
				Expect(err).NotTo(HaveOccurred())
				lines := strings.Split(strings.TrimSpace(stdout), "\n")
				Expect(lines).To(HaveLen(2))
				Expect(lines[1]).To(HaveSuffix(wantSuffix))
			},
			Entry("a job that recorded both labels", true, true, ",false,true,true"),
			Entry("a job from before snapshots", false, false, ",false,false,false"),
		)

		DescribeTable("says in JSON whether each side's label was recorded when the job was created",
			func(recorded bool) {
				fake.pages[0].Analysis.SourceCatalog.Snapshot = recorded
				fake.pages[0].Analysis.TargetCatalog.Snapshot = recorded
				stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
				Expect(err).NotTo(HaveOccurred())
				var out struct {
					Analysis exportedAnalysis `json:"analysis"`
				}
				Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed())
				Expect(out.Analysis.Source).To(HaveKeyWithValue("snapshot", recorded))
				Expect(out.Analysis.Target).To(HaveKeyWithValue("snapshot", recorded))
			},
			Entry("a job that recorded both labels", true),
			Entry("a job from before snapshots", false),
		)

		It("takes the analysis from the first page when the export spans pages", func() {
			first := fake.pages[0]
			first.PageInfo = &pb.PageInfo{NextPageToken: "tok-2"}
			fake.pages = append(fake.pages, mappingsPage("", mapping("b", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF)))
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
			Expect(err).NotTo(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(HaveLen(3))
			Expect(lines[2]).To(HaveSuffix(",cat-src,CSF 2.0,2.0,cat-tgt,NIST SP 800-53,,false,true,true"))
		})

		It("guards uploader-derived CSV cells against formulas, leaving server-generated ones alone", func() {
			fake.pages[0].Mappings[0].SourceControlId = "=cmd|' /C calc'!A0"
			fake.pages[0].Mappings[0].TargetControlId = "-AC-1"
			fake.pages[0].Analysis.SourceCatalog.CatalogId = "@cat"
			fake.pages[0].Analysis.SourceCatalog.Name = "+CSF"
			fake.pages[0].Analysis.SourceCatalog.Version = "-2.0"
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
			Expect(err).NotTo(HaveOccurred())
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(HaveLen(2))
			Expect(lines[1]).To(Equal(`a,'=cmd|' /C calc'!A0,'-AC-1,RELATIONSHIP_TYPE_EQUIVALENT,0.900,0.800,true,` +
				`'@cat,'+CSF,'-2.0,cat-tgt,NIST SP 800-53,,false,true,true`))
		})

		It("keeps formula characters as stored in JSON", func() {
			fake.pages[0].Mappings[0].TargetControlId = "-AC-1"
			fake.pages[0].Analysis.SourceCatalog.Name = "=CSF"
			stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
			Expect(err).NotTo(HaveOccurred())
			var out struct {
				Analysis exportedAnalysis `json:"analysis"`
				Mappings []map[string]any `json:"mappings"`
			}
			Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed())
			Expect(out.Analysis.Source).To(HaveKeyWithValue("name", "=CSF"))
			Expect(out.Mappings[0]).To(HaveKeyWithValue("targetControlId", "-AC-1"))
		})

		Describe("an uploader-controlled catalog name and version", func() {
			BeforeEach(func() {
				fake.pages[0].Analysis.SourceCatalog.Name = "Evil\x1b]0;pwned\x07\nCatalog"
				fake.pages[0].Analysis.SourceCatalog.Version = "1\u202e0"
			})

			It("exports them to CSV as one clean line", func() {
				stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "csv")
				Expect(err).NotTo(HaveOccurred())
				lines := strings.Split(strings.TrimSpace(stdout), "\n")
				Expect(lines).To(HaveLen(2))
				Expect(lines[1]).To(HaveSuffix(",cat-src,Evil ]0;pwned Catalog,10,cat-tgt,NIST SP 800-53,,false,true,true"))
				Expect(stdout).NotTo(ContainSubstring("\x1b"))
			})

			It("exports them to JSON as stored, with control characters escaped", func() {
				stdout, _, err := execute(newCmd(), "export", "job-1", "--format", "json")
				Expect(err).NotTo(HaveOccurred())
				Expect(stdout).NotTo(ContainSubstring("\x1b"))
				var out struct {
					Analysis exportedAnalysis `json:"analysis"`
				}
				Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed())
				Expect(out.Analysis.Source).To(SatisfyAll(
					HaveKeyWithValue("name", "Evil\x1b]0;pwned\x07\nCatalog"), HaveKeyWithValue("version", "1\u202e0")))
			})
		})
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

	It("says where to see which analyzers ran when a summary has no mappings", func() {
		fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("")}
		out, _, err := execute(newCmd(), "summary", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Total mappings: 0"))
		Expect(out).To(ContainSubstring(`see which analyzers the job ran with "crosscodex run status job-1"`))
	})

	It("gives no analyzer hint when a summary has mappings", func() {
		fake.pages = []*pb.ListJobMappingsResponse{mappingsPage("", mapping("m1", pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT))}
		out, _, err := execute(newCmd(), "summary", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("crosscodex run status"))
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
		var (
			dir    string
			pubKey string
			gen    attestation.Generator
			layout *attestation.SignedLayout
			linkA  *attestation.SignedLink
			linkB  *attestation.SignedLink
		)

		writeFile := func(name string, data []byte) string {
			path := filepath.Join(dir, name)
			Expect(os.WriteFile(path, data, 0o600)).To(Succeed())
			return path
		}
		bundleFile := func(links ...*attestation.SignedLink) string {
			data, err := attestation.MarshalBundle(layout, links)
			Expect(err).NotTo(HaveOccurred())
			return writeFile("bundle.json", data)
		}

		// run is the job the bundle is bound to, as the pipeline binds it.
		run := attestation.Artifact{URI: "job:acme/job-1", Digest: "sha256:cfg"}

		// A two-step job attestation signed with a key pair whose public
		// half is in pubKey, as the pipeline stores it.
		BeforeEach(func() {
			dir = GinkgoT().TempDir()
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			Expect(err).NotTo(HaveOccurred())
			privDER, err := x509.MarshalECPrivateKey(key)
			Expect(err).NotTo(HaveOccurred())
			pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
			Expect(err).NotTo(HaveOccurred())
			privKey := writeFile("key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}))
			pubKey = writeFile("pub.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
			kp, err := attestation.LoadFileKeys(privKey, pubKey, false)
			Expect(err).NotTo(HaveOccurred())
			gen, err = attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())

			ctx := context.Background()
			layout, err = gen.CreateLayout(ctx, attestation.LayoutOptions{
				Steps: []attestation.Step{
					{Name: "classify", ExpectedMaterials: []string{run.URI}, ExpectedProducts: []string{"classify.output"}, Threshold: 1},
					{Name: "synthesis", ExpectedMaterials: []string{run.URI, "classify.output"}, ExpectedProducts: []string{"synthesis.output"}, Threshold: 1},
				},
				ExpiresIn: time.Hour,
			})
			Expect(err).NotTo(HaveOccurred())
			out := []attestation.Artifact{{URI: "classify.output", Digest: "sha256:aaa"}}
			linkA, err = gen.CreateLink(ctx, "classify", []attestation.Artifact{run}, out)
			Expect(err).NotTo(HaveOccurred())
			linkB, err = gen.CreateLink(ctx, "synthesis", append([]attestation.Artifact{run}, out...), []attestation.Artifact{{URI: "synthesis.output", Digest: "sha256:bbb"}})
			Expect(err).NotTo(HaveOccurred())
		})

		It("verifies a job's bundle: layout, every step's link and the chain, listing the steps and the signed job", func() {
			stdout, _, err := execute(newRootCmd(), "results", "verify", bundleFile(linkA, linkB), "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(And(ContainSubstring("Status: Verified"), ContainSubstring("classify"), ContainSubstring("synthesis"),
				ContainSubstring("Tenant: acme"), ContainSubstring("Job: job-1"), ContainSubstring("Job config digest: sha256:cfg")))
		})

		It("fails when the bundle was signed for another job, naming the job it expected", func() {
			_, _, err := execute(newRootCmd(), "results", "verify", bundleFile(linkA, linkB), "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-2")
			Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
			Expect(err.Error()).To(ContainSubstring("job:acme/job-2"))
		})

		DescribeTable("refuses to run without the job it verifies, verifying nothing",
			func(args ...string) {
				stdout, _, err := execute(newRootCmd(), append([]string{"results", "verify", bundleFile(linkA, linkB), "--public-key", pubKey}, args...)...)
				Expect(err).To(MatchError(And(ContainSubstring("--tenant"), ContainSubstring("--job-id"))))
				Expect(stdout).NotTo(ContainSubstring("Verified"))
			},
			Entry("without --job-id", "--tenant", "acme"),
			Entry("without --tenant", "--job-id", "job-1"),
		)

		It("reports the verified steps as JSON", func() {
			stdout, _, err := execute(newRootCmd(), "--json", "results", "verify", bundleFile(linkA, linkB), "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).NotTo(HaveOccurred())
			var got map[string]any
			Expect(json.Unmarshal([]byte(stdout), &got)).To(Succeed())
			Expect(got).To(HaveKeyWithValue("status", "verified"))
			Expect(got).To(HaveKeyWithValue("steps", ConsistOf("classify", "synthesis")))
			Expect(got).To(HaveKeyWithValue("tenant", "acme"))
			Expect(got).To(HaveKeyWithValue("job_id", "job-1"))
			Expect(got).To(HaveKeyWithValue("job_config_digest", "sha256:cfg"))
		})

		It("fails, naming the step, when a step's link is missing", func() {
			_, _, err := execute(newRootCmd(), "results", "verify", bundleFile(linkA), "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).To(MatchError(attestation.ErrMissingLink))
			Expect(err.Error()).To(ContainSubstring(`"synthesis"`))
		})

		It("fails, naming the step, when a link was signed with another key", func() {
			otherKP, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			other, err := attestation.NewGenerator(otherKP)
			Expect(err).NotTo(HaveOccurred())
			foreign, err := other.CreateLink(context.Background(), "synthesis", linkB.Materials, linkB.Products)
			Expect(err).NotTo(HaveOccurred())

			_, _, err = execute(newRootCmd(), "results", "verify", bundleFile(linkA, foreign), "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).To(MatchError(attestation.ErrVerificationFailed))
			Expect(err.Error()).To(ContainSubstring(`"synthesis"`))
		})

		It("refuses a single link envelope, saying which file to pass", func() {
			path := writeFile("link.json", linkA.Raw)
			_, _, err := execute(newRootCmd(), "results", "verify", path, "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).To(MatchError(attestation.ErrInvalidBundle))
			Expect(err.Error()).To(ContainSubstring("attestations/<tenant>/<job-id>/bundle.json"))
		})

		It("refuses a file that is not JSON", func() {
			path := writeFile("bundle.json", []byte("not json"))
			_, _, err := execute(newRootCmd(), "results", "verify", path, "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).To(MatchError(attestation.ErrInvalidBundle))
		})

		It("refuses to run without --public-key, verifying nothing", func() {
			stdout, _, err := execute(newRootCmd(), "results", "verify", bundleFile(linkA, linkB))
			Expect(err).To(MatchError(ContainSubstring("--public-key")))
			Expect(err.Error()).To(ContainSubstring("attestation.public_key_path"))
			Expect(stdout).NotTo(ContainSubstring("Verified"))
		})

		It("errors when bundle file does not exist", func() {
			_, _, err := execute(newRootCmd(), "results", "verify", "/nonexistent/bundle.json", "--public-key", pubKey, "--tenant", "acme", "--job-id", "job-1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("read bundle file"))
		})
	})
})

// fakeCandidatesClient serves ListJobCandidates from a queue of pages and
// records every request; other GatewayServiceClient methods panic through
// the nil embed.
type fakeCandidatesClient struct {
	crosscodexv1connect.GatewayServiceClient
	pages []*pb.ListJobCandidatesResponse
	calls []*pb.ListJobCandidatesRequest
}

func (f *fakeCandidatesClient) ListJobCandidates(_ context.Context, req *connectrpc.Request[pb.ListJobCandidatesRequest]) (*connectrpc.Response[pb.ListJobCandidatesResponse], error) {
	// Copy: the CLI mutates one request across pages.
	f.calls = append(f.calls, &pb.ListJobCandidatesRequest{
		JobId:      req.Msg.GetJobId(),
		ControlId:  req.Msg.GetControlId(),
		Pagination: &pb.Pagination{PageSize: req.Msg.GetPagination().GetPageSize(), PageToken: req.Msg.GetPagination().GetPageToken()},
	})
	return connectrpc.NewResponse(f.pages[len(f.calls)-1]), nil
}

func candidatesPage(next string, analysis *pb.JobAnalysis, cs ...*pb.RelationshipCandidate) *pb.ListJobCandidatesResponse {
	return &pb.ListJobCandidatesResponse{Candidates: cs, PageInfo: &pb.PageInfo{NextPageToken: next}, Analysis: analysis}
}

func candidate(src, tgt string, score float32, generators ...string) *pb.RelationshipCandidate {
	c := &pb.RelationshipCandidate{SourceControlId: src, TargetControlId: tgt, SimilarityScore: score}
	for _, g := range generators {
		c.Contributions = append(c.Contributions, &pb.CandidateContribution{Generator: g, Score: 0.5, Weight: 1})
	}
	return c
}

var _ = Describe("results candidates", func() {
	var fake *fakeCandidatesClient

	run := func(args ...string) (string, error) {
		cmd := newResultsCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		stdout, _, err := execute(cmd, append([]string{"candidates"}, args...)...)
		return stdout, err
	}

	BeforeEach(func() {
		fake = &fakeCandidatesClient{pages: []*pb.ListJobCandidatesResponse{candidatesPage("", nil)}}
	})

	It("exports JSON across pages with every field present", func() {
		analysis := &pb.JobAnalysis{Bidirectional: true}
		fake.pages = []*pb.ListJobCandidatesResponse{
			candidatesPage("p2", analysis, candidate("a", "b", 0.75, "bm25", "dense")),
			candidatesPage("", nil, candidate("c", "d", 0)),
		}
		stdout, err := run("job-1", "--format", "json")
		Expect(err).NotTo(HaveOccurred())

		var out struct {
			JobID      string           `json:"job_id"`
			Analysis   map[string]any   `json:"analysis"`
			Candidates []map[string]any `json:"candidates"`
		}
		Expect(json.Unmarshal([]byte(stdout), &out)).To(Succeed(), "valid JSON: %s", stdout)
		Expect(out.JobID).To(Equal("job-1"))
		Expect(out.Analysis).To(HaveKeyWithValue("bidirectional", true), "the first page's analysis is kept")
		Expect(out.Candidates).To(HaveLen(2))
		Expect(out.Candidates[0]).To(HaveKeyWithValue("sourceControlId", "a"))
		Expect(out.Candidates[0]).To(HaveKeyWithValue("targetControlId", "b"))
		Expect(out.Candidates[0]).To(HaveKeyWithValue("similarityScore", 0.75))
		Expect(out.Candidates[0]).To(HaveKeyWithValue("contributions", HaveLen(2)))
		Expect(out.Candidates[0]["contributions"].([]any)[0]).To(And(
			HaveKeyWithValue("generator", "bm25"), HaveKey("score"), HaveKey("weight"), HaveKey("metadata")))
		Expect(out.Candidates[1]).To(HaveKeyWithValue("similarityScore", 0.0), "a zero score is exported, not omitted")
		Expect(out.Candidates[1]).To(HaveKeyWithValue("contributions", BeEmpty()))
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetPagination().GetPageSize()).To(Equal(int32(1000)))
		Expect(fake.calls[1].GetPagination().GetPageToken()).To(Equal("p2"))
	})

	It("writes null analysis when the response carries none", func() {
		stdout, err := run("job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring(`"analysis": null`))
		Expect(stdout).To(ContainSubstring(`"candidates": []`))
	})

	It("sends --control in the request", func() {
		_, err := run("job-1", "--control", "AC-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetJobId()).To(Equal("job-1"))
		Expect(fake.calls[0].GetControlId()).To(Equal("AC-1"))
	})

	It("exports CSV with generators joined by semicolons and catalog IDs made spreadsheet-safe", func() {
		fake.pages = []*pb.ListJobCandidatesResponse{
			candidatesPage("", nil, candidate("-AC-1", "t-1", 0.5, "bm25", "dense"), candidate("a", "b", 0.25)),
		}
		stdout, err := run("job-1", "--format", "csv")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Split(strings.TrimSpace(stdout), "\n")).To(Equal([]string{
			"source_control_id,target_control_id,similarity_score,generators",
			"'-AC-1,t-1,0.500,bm25;dense",
			"a,b,0.250,",
		}))
	})

	It("writes to --output instead of stdout", func() {
		path := filepath.Join(GinkgoT().TempDir(), "cand.json")
		stdout, err := run("job-1", "--output", path)
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(BeEmpty())
		b, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring(`"job_id": "job-1"`))
	})

	It("stops when the server repeats the page token it was given", func() {
		fake.pages = []*pb.ListJobCandidatesResponse{candidatesPage("tok", nil), candidatesPage("tok", nil), candidatesPage("tok", nil)}
		_, err := run("job-1")
		Expect(err).To(MatchError(And(
			ContainSubstring(`list candidates for job job-1: server returned page token "tok" again`),
			ContainSubstring("crosscodexd bug"))))
		Expect(fake.calls).To(HaveLen(2))
	})

	It("rejects an unknown format, listing the valid ones, without calling the server", func() {
		_, err := run("job-1", "--format", "xml")
		Expect(err).To(MatchError(ContainSubstring(`invalid --format "xml": use one of json, csv`)))
		Expect(fake.calls).To(BeEmpty())
	})

	It("asks for a job id when none is given", func() {
		_, err := run()
		Expect(err).To(MatchError(ContainSubstring("job-id")))
	})
})
