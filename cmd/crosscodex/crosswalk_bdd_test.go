package main

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	"github.com/xuri/excelize/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeCrosswalkClient serves GetCrosswalk from a queue of pages and records
// every request; other GatewayServiceClient methods panic through the nil
// embed.
type fakeCrosswalkClient struct {
	crosscodexv1connect.GatewayServiceClient
	pages []*pb.GetCrosswalkResponse
	err   error
	calls []*pb.GetCrosswalkRequest
}

func (f *fakeCrosswalkClient) GetCrosswalk(_ context.Context, req *connectrpc.Request[pb.GetCrosswalkRequest]) (*connectrpc.Response[pb.GetCrosswalkResponse], error) {
	// Clone: the CLI mutates one request across pages.
	f.calls = append(f.calls, proto.Clone(req.Msg).(*pb.GetCrosswalkRequest))
	if f.err != nil {
		return nil, f.err
	}
	return connectrpc.NewResponse(f.pages[len(f.calls)-1]), nil
}

// pagedFailClient serves the wrapped fake's first failAfter pages, then fails.
type pagedFailClient struct {
	*fakeCrosswalkClient
	failAfter int
	err       error
	calls     *int
}

func (c *pagedFailClient) GetCrosswalk(ctx context.Context, req *connectrpc.Request[pb.GetCrosswalkRequest]) (*connectrpc.Response[pb.GetCrosswalkResponse], error) {
	*c.calls++
	if *c.calls > c.failAfter {
		return nil, c.err
	}
	return c.fakeCrosswalkClient.GetCrosswalk(ctx, req)
}

var crosswalkProvenance = &pb.CrosswalkProvenance{
	Mode:  "pair",
	Stage: pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED,
	Catalogs: []*pb.CatalogSummary{
		{CatalogId: "cat-a", Name: "Framework A", Version: "1.0", Imported: true, Snapshot: true},
		{CatalogId: "cat-b", Name: "Framework B", Version: "2", Imported: true},
	},
	JobIds:        []string{"job-1", "job-2"},
	Rows:          2,
	ServerVersion: "test",
	GeneratedAt:   timestamppb.New(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)),
}

// crosswalkRows has one ordinary row and one whose titles carry the
// characters CSV and spreadsheets treat specially.
func crosswalkRows() []*pb.CrosswalkRow {
	return []*pb.CrosswalkRow{
		{
			Source:       &pb.CrosswalkSide{NodeId: "cat-a/ac-1", CatalogId: "cat-a", Framework: "Framework A", Identifier: "AC-1", ParentIdentifier: "AC", Title: "Access Control Policy", Statement: "Define access rules."},
			Target:       &pb.CrosswalkSide{NodeId: "cat-b/3.1.1", CatalogId: "cat-b", Framework: "Framework B", Identifier: "3.1.1", ParentIdentifier: "3.1", Title: "Authorized access", Statement: "Limit access to authorized users."},
			Relationship: "equivalent", Confidence: 0.9, Direction: "direct",
			JobsJudged: 2, JobsAgreeing: 2, LatestJobId: "job-2",
		},
		{
			Source:       &pb.CrosswalkSide{NodeId: "cat-a/ac-2", CatalogId: "cat-a", Framework: "Framework A", Identifier: "AC-2", Title: "=HYPERLINK(\"http://x\")\nsecond | line, \"quoted\"", Statement: "Manage accounts."},
			Target:       &pb.CrosswalkSide{NodeId: "cat-b/3.1.2", CatalogId: "cat-b", Framework: "Framework B", Identifier: "3.1.2", ParentIdentifier: "3.1", Title: "Account management", Statement: "Manage system accounts."},
			Relationship: "subset of", Confidence: 0.75, Direction: "direct",
			JobsJudged: 2, JobsAgreeing: 1, OtherVerdicts: "job-1: complements", LatestJobId: "job-2",
		},
	}
}

func crosswalkPage(next string, rows ...*pb.CrosswalkRow) *pb.GetCrosswalkResponse {
	return &pb.GetCrosswalkResponse{Rows: rows, PageInfo: &pb.PageInfo{NextPageToken: next}, Provenance: crosswalkProvenance}
}

var _ = Describe("crosscodex crosswalk", func() {
	var (
		fake  *fakeCrosswalkClient
		state *cliState
	)

	run := func(args ...string) (string, string, error) {
		root := &cobra.Command{Use: "crosscodex", SilenceUsage: true, SilenceErrors: true}
		root.AddCommand(newCrosswalkCmd(state))
		return execute(root, append([]string{"crosswalk"}, args...)...)
	}

	BeforeEach(func() {
		fake = &fakeCrosswalkClient{pages: []*pb.GetCrosswalkResponse{crosswalkPage("", crosswalkRows()...)}}
		state = &cliState{client: fake}
	})

	DescribeTable("builds the request for each mode",
		func(args []string, check func(*pb.GetCrosswalkRequest)) {
			_, _, err := run(args...)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(HaveLen(1))
			check(fake.calls[0])
		},
		Entry("pair is the default for two catalogs", []string{"cat-a", "cat-b"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetPair().GetCatalogA()).To(Equal("cat-a"))
			Expect(r.GetPair().GetCatalogB()).To(Equal("cat-b"))
		}),
		Entry("job", []string{"--job", "job-9"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetJobId()).To(Equal("job-9"))
		}),
		Entry("one catalog against all", []string{"cat-a", "--all"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetOneToManyCatalogId()).To(Equal("cat-a"))
		}),
		Entry("transitive", []string{"cat-a", "cat-c", "--via", "hub"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetTransitive().GetCatalogA()).To(Equal("cat-a"))
			Expect(r.GetTransitive().GetCatalogC()).To(Equal("cat-c"))
			Expect(r.GetTransitive().GetHubCatalogId()).To(Equal("hub"))
		}),
		Entry("defaults", []string{"cat-a", "cat-b"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetStage()).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED))
			Expect(r.GetMinConfidence()).To(BeZero())
			Expect(r.GetRelationshipTypes()).To(BeEmpty())
			Expect(r.GetIncludeNonViable()).To(BeFalse())
			Expect(r.GetPagination().GetPageSize()).To(Equal(int32(1000)))
		}),
		Entry("include non-viable", []string{"cat-a", "cat-b", "--include-non-viable"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetIncludeNonViable()).To(BeTrue())
		}),
		Entry("filters", []string{"cat-a", "cat-b", "--stage", "Analyzer", "--min-confidence", "0.5", "--type", "equivalent", "--type", "SUBSET_OF"}, func(r *pb.GetCrosswalkRequest) {
			Expect(r.GetStage()).To(Equal(pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER))
			Expect(r.GetMinConfidence()).To(BeNumerically("~", 0.5, 1e-6))
			Expect(r.GetRelationshipTypes()).To(Equal([]pb.RelationshipType{
				pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
				pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF,
			}))
		}),
	)

	DescribeTable("rejects bad arguments with an actionable error and never calls the server",
		func(want string, args ...string) {
			_, _, err := run(args...)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(fake.calls).To(BeEmpty())
		},
		Entry("--via with one catalog", "--via needs two catalogs: crosscodex crosswalk <a> <c> --via <hub>", "cat-a", "--via", "hub"),
		Entry("--job with catalogs", "--job takes no catalogs", "cat-a", "--job", "job-1"),
		Entry("--all with two catalogs", "--all needs exactly one catalog", "cat-a", "cat-b", "--all"),
		Entry("--all with no catalog", "--all needs exactly one catalog", "--all"),
		Entry("--job with --all", "pass only one of them", "--job", "job-1", "--all"),
		Entry("--all with --via", "pass only one of them", "cat-a", "--all", "--via", "hub"),
		Entry("one catalog and no mode", "choose what to export", "cat-a"),
		Entry("no arguments", "choose what to export"),
		Entry("three catalogs", "requires", "a", "b", "c"),
		Entry("unknown stage", "synthesized, analyzer", "a", "b", "--stage", "bogus"),
		Entry("unknown type", "equivalent", "a", "b", "--type", "bogus"),
		Entry("confidence above 1", "between 0 and 1", "a", "b", "--min-confidence", "1.5"),
		Entry("unknown format", `invalid --format "xml": use one of csv, json, md, xlsx`, "a", "b", "--format", "xml"),
		Entry("non-viable with the analyzer stage", "--include-non-viable only applies to the synthesized stage", "a", "b", "--stage", "analyzer", "--include-non-viable"),
		Entry("no_relationship type", "no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only", "a", "b", "--type", "no_relationship"),
	)

	It("reads every page and keeps the first page's provenance", func() {
		rows := crosswalkRows()
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("tok", rows[0]), crosswalkPage("", rows[1])}
		stdout, stderr, err := run("cat-a", "cat-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(2))
		Expect(fake.calls[0].GetPagination().GetPageToken()).To(BeEmpty())
		Expect(fake.calls[1].GetPagination().GetPageToken()).To(Equal("tok"))
		Expect(stderr).To(ContainSubstring("2 rows from 2 completed jobs"))

		records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(stdout, "\xEF\xBB\xBF"))).ReadAll()
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(3))
	})

	It("stops when the server repeats the page token it was given", func() {
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("tok"), crosswalkPage("tok"), crosswalkPage("tok")}
		_, _, err := run("cat-a", "cat-b")
		Expect(err).To(MatchError(ContainSubstring(`server returned page token "tok" again`)))
		Expect(err).To(MatchError(ContainSubstring("crosscodexd bug")))
		Expect(fake.calls).To(HaveLen(2))
	})

	DescribeTable("writes the expected bytes",
		func(format, golden string) {
			stdout, _, err := run("cat-a", "cat-b", "--format", format)
			Expect(err).NotTo(HaveOccurred())
			want, err := os.ReadFile(filepath.Join("testdata", "crosswalk", golden))
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(Equal(string(want)))
		},
		Entry("CSV with BOM, header and quoted cells", "csv", "pair.csv"),
		Entry("JSON with provenance and ordered keys", "json", "pair.json"),
		Entry("Markdown with cleaned line breaks, escaped pipes and parentheses and no formula quote", "md", "pair.md"),
	)

	It("writes the target framework of each row when one catalog is mapped against several", func() {
		rows := crosswalkRows()
		rows[1].Target.NodeId, rows[1].Target.CatalogId, rows[1].Target.Framework = "cat-c/3.1.2", "cat-c", "Framework C"
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", rows...)}
		stdout, _, err := run("cat-a", "--all")
		Expect(err).NotTo(HaveOccurred())
		want, err := os.ReadFile(filepath.Join("testdata", "crosswalk", "one_to_many.csv"))
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal(string(want)))
	})

	It("writes direct and derived rows of a transitive crosswalk, naming the hub control and the unknown type", func() {
		direct := crosswalkRows()[0]
		direct.Target = &pb.CrosswalkSide{NodeId: "cat-c/3.1.1", CatalogId: "cat-c", Framework: "Framework C", Identifier: "3.1.1", ParentIdentifier: "3.1", Title: "Authorized access", Statement: "Limit access to authorized users."}
		viaHub1 := proto.Clone(direct).(*pb.CrosswalkRow)
		viaHub1.Relationship, viaHub1.RelationshipType = "subset of", pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF
		viaHub1.Confidence, viaHub1.Direction, viaHub1.HubIdentifier = 0.4, "derived", "HB-1"
		viaHub1.JobsJudged, viaHub1.JobsAgreeing, viaHub1.LatestJobId = 3, 3, "job-4"
		viaHub2 := proto.Clone(viaHub1).(*pb.CrosswalkRow)
		viaHub2.Relationship, viaHub2.RelationshipType = "related (derived, type unknown)", pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
		viaHub2.Confidence, viaHub2.HubIdentifier = 0.3, "HB-2"
		viaHub2.JobsJudged, viaHub2.JobsAgreeing, viaHub2.LatestJobId = 2, 2, "job-3"
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", direct, viaHub1, viaHub2)}

		stdout, _, err := run("cat-a", "cat-c", "--via", "hub")
		Expect(err).NotTo(HaveOccurred())
		want, err := os.ReadFile(filepath.Join("testdata", "crosswalk", "transitive.csv"))
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(Equal(string(want)))
	})

	It("keeps one CSV record per row when a cell has a newline, quotes, a pipe and a leading equals", func() {
		stdout, _, err := run("cat-a", "cat-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(HavePrefix("\xEF\xBB\xBF"))
		records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(stdout, "\xEF\xBB\xBF"))).ReadAll()
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(3))
		Expect(records[0]).To(Equal(crosswalkColumns))
		Expect(records[2][3]).To(Equal(`'=HYPERLINK("http://x") second | line, "quoted"`))
		Expect(strings.Count(stdout, "\n")).To(Equal(3), "newlines in a cell are cleaned, so each record is one line")
	})

	It("keeps a long statement whole in CSV", func() {
		long := strings.Repeat("word ", 200)
		rows := crosswalkRows()
		rows[0].Source.Statement = strings.TrimSpace(long)
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", rows...)}
		stdout, _, err := run("cat-a", "cat-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring(strings.TrimSpace(long)))
	})

	It("cuts a Markdown cell over 200 characters and keeps the row on one line", func() {
		rows := crosswalkRows()
		rows[0].Source.Statement = strings.Repeat("é", 201) + "\n|"
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", rows...)}
		stdout, _, err := run("cat-a", "cat-b", "--format", "md")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("| " + strings.Repeat("é", 200) + "… |"))
		Expect(strings.Count(stdout, "\n")).To(Equal(4))
	})

	DescribeTable("neutralises control text in a Markdown cell and keeps the row on one table line",
		func(payload string, inert string, absent ...string) {
			rows := crosswalkRows()
			rows[0].Source.Title = payload
			fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", rows...)}
			stdout, _, err := run("cat-a", "cat-b", "--format", "md")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Count(stdout, "\n")).To(Equal(4), "header, separator and two rows")
			Expect(strings.Split(stdout, "\n")[2]).To(ContainSubstring(inert))
			for _, bad := range absent {
				Expect(stdout).NotTo(ContainSubstring(bad))
			}
		},
		Entry("an HTML comment cannot hide later rows", "a <!-- x --> b", `a &lt;\!-- x --&gt; b`, "<!--"),
		Entry("an image tag cannot act as a beacon", "<img src=x>", "&lt;img src=x&gt;", "<img"),
		Entry("a link cannot be formed", "[click](http://evil)", `\[click\]\(http://evil\)`, "[click]("),
		Entry("an image cannot be formed", "![i](u)", `\!\[i\]\(u\)`, "![i]("),
		Entry("an ANSI escape is removed", "x\x1b[31mred", `x \[31mred`, "\x1b"),
		Entry("a bidi override is removed", "ab\u202ecd", "abcd", "\u202e"),
		Entry("a zero-width space is removed", "ab\u200bcd", "abcd", "\u200b"),
		Entry("an ampersand entity is not decoded", "&lt;b&gt;", "&amp;lt;b&amp;gt;"),
		Entry("emphasis and code markers are escaped", "*a* _b_ `c`", "\\*a\\* \\_b\\_ \\`c\\`"),
	)

	Describe("XLSX", func() {
		var book *excelize.File

		openBook := func() {
			path := filepath.Join(GinkgoT().TempDir(), "crosswalk.xlsx")
			stdout, _, err := run("cat-a", "cat-b", "--format", "xlsx", "--output", path)
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(BeEmpty())
			book, err = excelize.OpenFile(path)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(book.Close)
		}

		It("refuses to write a binary workbook to stdout and does not call the server", func() {
			stdout, _, err := run("cat-a", "cat-b", "--format", "xlsx")
			Expect(err).To(MatchError("xlsx output is binary: pass --output <file>.xlsx"))
			Expect(stdout).To(BeEmpty())
			Expect(fake.calls).To(BeEmpty())
		})

		It("cuts a statement over the cell capacity with a closing ellipsis and keeps a shorter one whole", func() {
			rows := crosswalkRows()
			capacity := excelize.TotalCellChars
			rows[0].Source.Statement = strings.Repeat("a", capacity+10)
			rows[1].Source.Statement = strings.Repeat("b", capacity)
			// Astral runes take two UTF-16 units, so the cut must not split one.
			rows[1].Target.Statement = strings.Repeat("\U0001F600", capacity)
			fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("", rows...)}
			openBook()
			sheet, err := book.GetRows("crosswalk")
			Expect(err).NotTo(HaveOccurred())
			Expect(sheet[1][4]).To(Equal(strings.Repeat("a", capacity-1) + "…"))
			Expect(sheet[2][4]).To(Equal(strings.Repeat("b", capacity)))
			Expect(sheet[2][13]).To(Equal(strings.Repeat("\U0001F600", (capacity-1)/2) + "…"))
		})

		It("has the crosswalk, provenance and disagreements sheets in order", func() {
			openBook()
			Expect(book.GetSheetList()).To(Equal([]string{"crosswalk", "provenance", "disagreements"}))
		})

		It("puts the columns and every row on the crosswalk sheet with a frozen header and an autofilter", func() {
			openBook()
			rows, err := book.GetRows("crosswalk")
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(3))
			Expect(rows[0]).To(Equal(crosswalkColumns))
			Expect(rows[1][1]).To(Equal("AC-1"))
			Expect(rows[1][6]).To(Equal("0.90"))

			panes, err := book.GetPanes("crosswalk")
			Expect(err).NotTo(HaveOccurred())
			Expect(panes.Freeze).To(BeTrue())
			Expect(panes.YSplit).To(Equal(1))
			Expect(panes.TopLeftCell).To(Equal("A2"))
			names := book.GetDefinedName()
			Expect(names).To(ContainElement(And(
				HaveField("Name", "_xlnm._FilterDatabase"),
				HaveField("Scope", "crosswalk"),
				HaveField("RefersTo", ContainSubstring("$A$1:$T$3")),
			)))
		})

		It("records the provenance as field and value rows", func() {
			openBook()
			rows, err := book.GetRows("provenance")
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0]).To(Equal([]string{"field", "value"}))
			Expect(rows).To(ContainElements(
				[]string{"mode", "pair"},
				[]string{"stage", "synthesized"},
				[]string{"rows", "2"},
				[]string{"generated_at", "2026-11-08T12:00:00Z"},
				[]string{"catalog", "cat-a | Framework A | 1.0 | imported=true | snapshot=true"},
				[]string{"job_id", "job-1"},
				[]string{"job_id", "job-2"},
			))
		})

		It("lists only rows where fewer jobs agreed than judged on the disagreements sheet", func() {
			openBook()
			rows, err := book.GetRows("disagreements")
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
			Expect(rows[0]).To(Equal(crosswalkColumns))
			Expect(rows[1][1]).To(Equal("AC-2"))
			Expect(rows[1][16]).To(Equal("job-1: complements"))
		})

		It("stores a title starting with = as text beginning with ' and no cell holds a formula", func() {
			openBook()
			title, err := book.GetCellValue("crosswalk", "D3")
			Expect(err).NotTo(HaveOccurred())
			Expect(title).To(HavePrefix(`'=HYPERLINK(`))
			for _, sheet := range book.GetSheetList() {
				rows, err := book.GetRows(sheet)
				Expect(err).NotTo(HaveOccurred())
				for i, row := range rows {
					for j := range row {
						cell, err := excelize.CoordinatesToCellName(j+1, i+1)
						Expect(err).NotTo(HaveOccurred())
						formula, err := book.GetCellFormula(sheet, cell)
						Expect(err).NotTo(HaveOccurred())
						Expect(formula).To(BeEmpty(), "%s!%s", sheet, cell)
						kind, err := book.GetCellType(sheet, cell)
						Expect(err).NotTo(HaveOccurred())
						Expect(kind).To(BeElementOf(excelize.CellTypeInlineString, excelize.CellTypeSharedString), "%s!%s", sheet, cell)
					}
				}
			}
		})
	})

	It("writes --output to a file and nothing to stdout", func() {
		path := filepath.Join(GinkgoT().TempDir(), "crosswalk.csv")
		stdout, _, err := run("cat-a", "cat-b", "--output", path)
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(BeEmpty())
		got, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		want, err := os.ReadFile(filepath.Join("testdata", "crosswalk", "pair.csv"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(string(want)))
	})

	It("tells the user what to run when the catalogs have no completed jobs", func() {
		empty := &pb.GetCrosswalkResponse{Provenance: &pb.CrosswalkProvenance{Mode: "pair"}}
		fake.pages = []*pb.GetCrosswalkResponse{empty}
		stdout, stderr, err := run("cat-a", "cat-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(HavePrefix("\xEF\xBB\xBF"))
		Expect(stderr).To(ContainSubstring("0 rows from 0 completed jobs"))
		Expect(stderr).To(ContainSubstring(`no completed jobs between cat-a and cat-b: run "crosscodex run start --source cat-a --target cat-b" first`))
	})

	DescribeTable("explains an empty crosswalk drawn from completed jobs",
		func(args []string, want ...string) {
			fake.pages = []*pb.GetCrosswalkResponse{{Provenance: &pb.CrosswalkProvenance{Mode: "pair", JobIds: []string{"job-1"}}}}
			_, stderr, err := run(args...)
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("0 rows from 1 completed jobs"))
			for _, w := range want {
				Expect(stderr).To(ContainSubstring(w))
			}
		},
		Entry("synthesized stage names the analyzer stage and the non-viable flag",
			[]string{"cat-a", "cat-b"}, "--stage analyzer", "--include-non-viable"),
		Entry("analyzer stage points at the filters",
			[]string{"cat-a", "cat-b", "--stage", "analyzer"}, "--min-confidence"),
	)

	It("surfaces a page-token mismatch unchanged and writes nothing", func() {
		fake.pages = []*pb.GetCrosswalkResponse{crosswalkPage("tok", crosswalkRows()[0])}
		stale := connectrpc.NewError(connectrpc.CodeInvalidArgument, errors.New("the crosswalk changed while paging (a job completed or was removed): re-run the export to get a consistent result"))
		calls := 0
		state.client = &pagedFailClient{fakeCrosswalkClient: fake, failAfter: 1, err: stale, calls: &calls}
		stdout, _, err := run("cat-a", "cat-b")
		Expect(err).To(MatchError(ContainSubstring("the crosswalk changed while paging (a job completed or was removed): re-run the export to get a consistent result")))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeInvalidArgument))
		Expect(stdout).To(BeEmpty())
	})

	It("reports mappings skipped for missing controls", func() {
		page := crosswalkPage("", crosswalkRows()...)
		page.Provenance = proto.Clone(crosswalkProvenance).(*pb.CrosswalkProvenance)
		page.Provenance.SkippedMissingControls = 3
		fake.pages = []*pb.GetCrosswalkResponse{page}
		_, stderr, err := run("cat-a", "cat-b")
		Expect(err).NotTo(HaveOccurred())
		Expect(stderr).To(ContainSubstring("3 mappings skipped"))
	})

	Describe("embedded mode", func() {
		const want = "crosswalks are read from the graph, which embedded mode does not run: connect to a crosscodexd daemon with --endpoint <host:port>"

		It("refuses before calling the server when the CLI runs the embedded daemon", func() {
			state.daemon = &embeddedDaemon{}
			stdout, _, err := run("cat-a", "cat-b")
			Expect(err).To(MatchError(want))
			Expect(stdout).To(BeEmpty())
			Expect(fake.calls).To(BeEmpty())
		})

		It("passes a remote daemon's Unavailable answer through instead of blaming embedded mode", func() {
			fake.err = connectrpc.NewError(connectrpc.CodeUnavailable, errors.New("graph service is down"))
			stdout, _, err := run("cat-a", "cat-b")
			Expect(err).To(MatchError(ContainSubstring("graph service is down")))
			Expect(err).NotTo(MatchError(ContainSubstring("embedded mode")))
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeUnavailable))
			Expect(stdout).To(BeEmpty())
		})

		It("passes other server errors through", func() {
			fake.err = connectrpc.NewError(connectrpc.CodeNotFound, errors.New("catalog cat-a not found"))
			_, _, err := run("cat-a", "cat-b")
			Expect(err).To(MatchError(ContainSubstring("catalog cat-a not found")))
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
		})
	})
})
