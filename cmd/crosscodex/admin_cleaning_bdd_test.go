package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stubCleaningClient implements crosscodexv1connect.EdgeCleaningServiceClient;
// like stubAdminClient it records each request and call count.
type stubCleaningClient struct {
	crosscodexv1connect.EdgeCleaningServiceClient

	runReq   *pb.RunCleaningRequest
	runResp  *pb.RunCleaningResponse
	runErr   error
	runCalls int

	listReq   *pb.ListFindingsRequest
	listResp  *pb.ListFindingsResponse
	listCalls int

	getReq   *pb.GetFindingRequest
	getResp  *pb.GetFindingResponse
	getCalls int

	decideReq   *pb.DecideFindingRequest
	decideResp  *pb.DecideFindingResponse
	decideCalls int
}

func (s *stubCleaningClient) RunCleaning(_ context.Context, req *connectrpc.Request[pb.RunCleaningRequest]) (*connectrpc.Response[pb.RunCleaningResponse], error) {
	return stubRPC(&s.runCalls, &s.runReq, s.runErr, s.runResp, req)
}

func (s *stubCleaningClient) ListFindings(_ context.Context, req *connectrpc.Request[pb.ListFindingsRequest]) (*connectrpc.Response[pb.ListFindingsResponse], error) {
	return stubRPC(&s.listCalls, &s.listReq, nil, s.listResp, req)
}

func (s *stubCleaningClient) GetFinding(_ context.Context, req *connectrpc.Request[pb.GetFindingRequest]) (*connectrpc.Response[pb.GetFindingResponse], error) {
	return stubRPC(&s.getCalls, &s.getReq, nil, s.getResp, req)
}

func (s *stubCleaningClient) DecideFinding(_ context.Context, req *connectrpc.Request[pb.DecideFindingRequest]) (*connectrpc.Response[pb.DecideFindingResponse], error) {
	return stubRPC(&s.decideCalls, &s.decideReq, nil, s.decideResp, req)
}

func cleaningTestFinding() *pb.Finding {
	return &pb.Finding{
		FindingId: "f-1", Signal: "conflicts_with", Status: "pending",
		EvidenceHash: "eh1", UpdatedAt: timestamppb.Now(),
		Detail: map[string]string{"zeta": "2", "alpha": "1"},
		Verdicts: []*pb.VerdictEvidence{{
			Ref:          &pb.VerdictRef{JobId: "j1", SourceNodeId: "n1", TargetNodeId: "n2"},
			Source:       &pb.ControlRef{Identifier: "ac-2", Label: "AC-2", Title: "Account Management", Catalog: "SP 800-53 r5"},
			Target:       &pb.ControlRef{Identifier: "pw.1", Label: "PW.1", Catalog: "SSDF 1.1"},
			Relationship: "equal",
			Agreement:    proto.Float64(0.5),
		}},
		Targets: []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "n1", TargetNodeId: "n2"}},
	}
}

// cleaningTestJobFinding is a retired job-level finding: it names its job
// and lists what the retire removed, but no verdicts.
func cleaningTestJobFinding() *pb.Finding {
	return &pb.Finding{
		FindingId: "f-job", Signal: "stale_prompt", Status: "retired", EvidenceHash: "eh2",
		Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "1", "analyzer.current": "2"},
		Job: &pb.FindingJob{
			JobId: "j1", CompletedAt: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			SourceCatalog: "SP 800-53 r5", TargetCatalog: "SSDF 1.1", VerdictsInScope: 2000,
		},
		Targets: []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "n1", TargetNodeId: "n2"}},
	}
}

const cleaningTestJobLine = "job j1: SP 800-53 r5 -> SSDF 1.1, completed 2026-01-01T00:00:00Z, 2000 verdict(s) in scope"

func cleaningTestRunResp() *pb.RunCleaningResponse {
	return &pb.RunCleaningResponse{
		PlanToken: "tok-1",
		Findings: []*pb.PlannedFinding{{
			Finding: cleaningTestFinding(), Action: "human", Change: "new", ToStatus: "pending",
		}},
		RetireVerdicts: 2,
		Summary:        &pb.CleaningSummary{Verdicts: 10, Pairs: 1, UnaddressableVerdicts: 3},
		Projected:      4,
	}
}

func newCleaningTestRoot(stub *stubCleaningClient) (*cliState, func(args ...string) (string, error)) {
	state := &cliState{cleaningClient: stub}
	return state, func(args ...string) (string, error) {
		stdout, _, err := execute(newAdminTestRoot(state), append([]string{"admin", "cleaning"}, args...)...)
		return stdout, err
	}
}

var _ = Describe("Admin Cleaning Commands", func() {
	var (
		stub *stubCleaningClient
		run  func(args ...string) (string, error)
	)
	BeforeEach(func() {
		stub = &stubCleaningClient{
			runResp: cleaningTestRunResp(),
			listResp: &pb.ListFindingsResponse{
				Findings: []*pb.Finding{cleaningTestFinding()},
				PageInfo: &pb.PageInfo{NextPageToken: "pg2"},
			},
			getResp: &pb.GetFindingResponse{
				Finding: cleaningTestFinding(), StateToken: "st-1",
				Decisions: []*pb.Decision{{
					DecisionId: "d1", Verdict: "dismiss", DecidedBy: "alice", RunId: "run-9",
					Rationale: "false positive", AttestationSha256: "abc123",
				}},
			},
			decideResp: &pb.DecideFindingResponse{Finding: &pb.Finding{FindingId: "f-1", Status: "dismissed"}, DecisionId: "d2", StateToken: "st-2"},
		}
		_, run = newCleaningTestRoot(stub)
	})

	Describe("run", func() {
		It("previews by default, names controls and catalogs, and ends with the apply command", func() {
			out, err := run("run", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.runReq.GetMode()).To(Equal(pb.CleaningMode_CLEANING_MODE_PREVIEW))
			Expect(stub.runReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
			Expect(stub.runReq.GetScope()).To(BeNil())
			Expect(out).To(ContainSubstring("new -> pending  conflicts_with"))
			Expect(out).To(ContainSubstring(`AC-2 "Account Management" [SP 800-53 r5] --equal--> PW.1 [SSDF 1.1]`))
			Expect(out).To(ContainSubstring("unaddressable verdicts: 3"))
			Expect(out).To(ContainSubstring("Verdicts to retire: 2"))
			Expect(out).To(HaveSuffix("To apply: crosscodex admin cleaning run --apply --expect tok-1 --tenant acme\n"))
		})

		It("names a job-level finding's job, catalogs, completion and verdict count", func() {
			stub.runResp.Findings = append(stub.runResp.Findings, &pb.PlannedFinding{
				Finding: cleaningTestJobFinding(), Action: "retire", Change: "new", ToStatus: "retired",
			})
			out, err := run("run", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("new -> retired  stale_prompt  finding f-job\n    " + cleaningTestJobLine + "\n"))
		})

		It("repeats the scope flags in the apply command", func() {
			out, err := run("run", "--tenant", "acme", "--source-catalog", "cat a", "--target-catalog", "cat-b")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.runReq.GetScope().GetSourceCatalogId()).To(Equal("cat a"))
			Expect(stub.runReq.GetScope().GetTargetCatalogId()).To(Equal("cat-b"))
			Expect(out).To(ContainSubstring("run --apply --expect tok-1 --source-catalog 'cat a' --target-catalog cat-b --tenant acme"))
		})

		It("omits the unaddressable line when there are none", func() {
			stub.runResp.Summary.UnaddressableVerdicts = 0
			out, err := run("run", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("unaddressable"))
		})

		It("applies with the token and reports what was applied, with no next-step line", func() {
			out, err := run("run", "--tenant", "acme", "--apply", "--expect", "tok-1")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.runReq.GetMode()).To(Equal(pb.CleaningMode_CLEANING_MODE_APPLY))
			Expect(stub.runReq.GetExpectToken()).To(Equal("tok-1"))
			Expect(out).To(ContainSubstring("Applied: 1 finding(s)"))
			Expect(out).NotTo(ContainSubstring("To apply:"))
		})

		It("resumes and prints the projected count", func() {
			out, err := run("run", "--tenant", "acme", "--resume")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.runReq.GetMode()).To(Equal(pb.CleaningMode_CLEANING_MODE_RESUME))
			Expect(out).To(ContainSubstring("Resumed: projected 4 recorded finding change(s) to the graph (retirements, restores and reviewer decisions)."))
		})

		It("emits snake_case proto keys as JSON", func() {
			out, err := run("run", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(out), &payload)).To(Succeed())
			Expect(payload).To(HaveKeyWithValue("plan_token", "tok-1"))
			Expect(payload).To(HaveKeyWithValue("retire_verdicts", BeEquivalentTo(2)))
			Expect(payload["summary"]).To(HaveKeyWithValue("unaddressable_verdicts", BeEquivalentTo(3)))
			planned := payload["findings"].([]any)[0].(map[string]any)
			Expect(planned).To(HaveKeyWithValue("to_status", "pending"))
			Expect(planned["finding"]).To(HaveKeyWithValue("finding_id", "f-1"))
		})

		It("keeps zero counts and empty lists in JSON", func() {
			stub.runResp = &pb.RunCleaningResponse{PlanToken: "tok-1"}
			out, err := run("run", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(out), &payload)).To(Succeed())
			Expect(payload).To(HaveKeyWithValue("findings", BeEmpty()))
			Expect(payload).To(HaveKeyWithValue("retire_verdicts", BeEquivalentTo(0)))
			Expect(payload).To(HaveKeyWithValue("projected", BeEquivalentTo(0)))
		})

		It("returns an RPC error unchanged", func() {
			stub.runErr = connectrpc.NewError(connectrpc.CodeUnimplemented, nil)
			_, err := run("run", "--tenant", "acme")
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeUnimplemented))
		})

		DescribeTable("refuses bad flags before any RPC",
			func(wantInErr string, args ...string) {
				_, err := run(append([]string{"run", "--tenant", "acme"}, args...)...)
				Expect(err).To(MatchError(ContainSubstring(wantInErr)))
				Expect(stub.runCalls).To(BeZero())
			},
			Entry("--apply without --expect", "--apply requires --expect <token>", "--apply"),
			Entry("--apply with --resume", "--apply and --resume cannot be combined", "--apply", "--expect", "t", "--resume"),
			Entry("--expect without --apply", "--expect is only valid with --apply", "--expect", "t"),
			Entry("--resume with a scope", "--resume cannot be combined with --source-catalog", "--resume", "--source-catalog", "a", "--target-catalog", "b"),
			Entry("only --source-catalog", "--source-catalog and --target-catalog go together", "--source-catalog", "a"),
			Entry("only --target-catalog", "--source-catalog and --target-catalog go together", "--target-catalog", "b"),
		)

		It("requires --tenant", func() {
			_, err := run("run")
			Expect(err).To(MatchError(ContainSubstring("--tenant")))
			Expect(stub.runCalls).To(BeZero())
		})
	})

	Describe("findings", func() {
		It("sends every filter and paging flag", func() {
			_, err := run("findings", "--tenant", "acme", "--signal", "conflicts_with", "--status", "pending",
				"--source-catalog", "a", "--target-catalog", "b", "--page-token", "pg1", "--page-size", "5")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.listReq.GetSignal()).To(Equal("conflicts_with"))
			Expect(stub.listReq.GetStatus()).To(Equal("pending"))
			Expect(stub.listReq.GetScope().GetSourceCatalogId()).To(Equal("a"))
			Expect(stub.listReq.GetScope().GetTargetCatalogId()).To(Equal("b"))
			Expect(stub.listReq.GetPagination().GetPageToken()).To(Equal("pg1"))
			Expect(stub.listReq.GetPagination().GetPageSize()).To(BeEquivalentTo(5))
		})

		It("lists readable rows and the exact next-page command", func() {
			out, err := run("findings", "--tenant", "acme", "--status", "pending")
			Expect(err).NotTo(HaveOccurred())

			Expect(out).To(ContainSubstring("f-1"))
			Expect(out).To(ContainSubstring("AC-2"))
			Expect(out).To(ContainSubstring("Next page: crosscodex admin cleaning findings --page-token pg2 --status pending --tenant acme"))
		})

		It("shows a job-level finding's job in the controls column", func() {
			stub.listResp.Findings = []*pb.Finding{cleaningTestJobFinding()}
			out, err := run("findings", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`f-job\s+stale_prompt\s+retired\s+\S*\s*` + regexp.QuoteMeta(cleaningTestJobLine)))
		})

		It("prints no next-page line on the last page", func() {
			stub.listResp.PageInfo = nil
			out, err := run("findings", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("Next page"))
		})

		It("says so when there are no findings", func() {
			stub.listResp = &pb.ListFindingsResponse{}
			out, err := run("findings", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("No findings."))
		})

		It("emits JSON with page_info", func() {
			out, err := run("findings", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(out), &payload)).To(Succeed())
			Expect(payload["page_info"]).To(HaveKeyWithValue("next_page_token", "pg2"))
			Expect(payload["findings"]).To(HaveLen(1))
		})

		It("refuses one catalog flag before any RPC", func() {
			_, err := run("findings", "--tenant", "acme", "--source-catalog", "a")
			Expect(err).To(MatchError(ContainSubstring("--source-catalog and --target-catalog go together")))
			Expect(stub.listCalls).To(BeZero())
		})
	})

	Describe("show", func() {
		It("prints evidence, history, the state token and the exact decide line", func() {
			out, err := run("show", "f-1", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.getReq.GetFindingId()).To(Equal("f-1"))
			Expect(out).To(ContainSubstring("key: j1:n1:n2"))
			Expect(out).To(ContainSubstring("agreement: 0.50"))
			Expect(strings.Index(out, "alpha: 1")).To(BeNumerically("<", strings.Index(out, "zeta: 2")))
			Expect(strings.Index(out, "alpha: 1")).To(BeNumerically(">=", 0))
			Expect(out).To(ContainSubstring("dismiss by alice"))
			Expect(out).To(ContainSubstring("rationale: false positive"))
			Expect(out).To(ContainSubstring("run: run-9"))
			Expect(out).To(ContainSubstring("attestation sha256: abc123"))
			Expect(out).To(ContainSubstring("State token: st-1"))
			Expect(out).To(HaveSuffix(`To decide: crosscodex admin cleaning decide f-1 --verdict <retire|dismiss|restore> --rationale "<text>" --expect st-1 --tenant acme` + "\n"))
		})

		It("prints a job-level finding's job and what its retire removed", func() {
			stub.getResp.Finding = cleaningTestJobFinding()
			out, err := run("show", "f-job", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("\nJob:\n  " + cleaningTestJobLine + "\n"))
			Expect(out).NotTo(ContainSubstring("Verdicts ("))
			Expect(out).To(ContainSubstring("Targets (retired when the finding is):\n  j1:n1:n2\n"))
		})

		It("emits JSON with state_token and decisions", func() {
			out, err := run("show", "f-1", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(out), &payload)).To(Succeed())
			Expect(payload).To(HaveKeyWithValue("state_token", "st-1"))
			Expect(payload["decisions"].([]any)[0]).To(HaveKeyWithValue("attestation_sha256", "abc123"))
		})

		It("requires exactly one finding ID", func() {
			_, err := run("show", "--tenant", "acme")
			Expect(err).To(HaveOccurred())
			Expect(stub.getCalls).To(BeZero())
		})
	})

	Describe("decide", func() {
		It("sends the decision with parsed retire-verdict refs", func() {
			out, err := run("decide", "f-1", "--tenant", "acme", "--verdict", "retire", "--rationale", "dup",
				"--expect", "st-1", "--retire-verdict", "j1:n1:n2", "--retire-verdict", "j2:n3:n4")
			Expect(err).NotTo(HaveOccurred())

			req := stub.decideReq
			Expect(req.GetFindingId()).To(Equal("f-1"))
			Expect(req.GetVerdict()).To(Equal("retire"))
			Expect(req.GetRationale()).To(Equal("dup"))
			Expect(req.GetExpectStateToken()).To(Equal("st-1"))
			Expect(req.GetRetireVerdicts()).To(HaveLen(2))
			Expect(req.GetRetireVerdicts()[1].GetSourceNodeId()).To(Equal("n3"))
			Expect(out).To(ContainSubstring("Recorded retire decision d2 on finding f-1."))
			Expect(out).To(ContainSubstring("Finding is now dismissed."))
			Expect(out).To(ContainSubstring("State token: st-2"))
		})

		It("emits JSON with decision_id", func() {
			out, err := run("decide", "f-1", "--tenant", "acme", "--verdict", "dismiss", "--rationale", "r", "--expect", "st-1", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(out), &payload)).To(Succeed())
			Expect(payload).To(HaveKeyWithValue("decision_id", "d2"))
			Expect(payload).To(HaveKeyWithValue("state_token", "st-2"))
		})

		DescribeTable("refuses bad flags before any RPC",
			func(wantInErr string, args ...string) {
				_, err := run(append([]string{"decide", "f-1", "--tenant", "acme"}, args...)...)
				Expect(err).To(MatchError(ContainSubstring(wantInErr)))
				Expect(stub.decideCalls).To(BeZero())
			},
			Entry("no --verdict", "--verdict is required", "--rationale", "r", "--expect", "t"),
			Entry("unknown --verdict", `--verdict "delete" is not valid`, "--verdict", "delete", "--rationale", "r", "--expect", "t"),
			Entry("no --rationale", "--rationale is required", "--verdict", "dismiss", "--expect", "t"),
			Entry("no --expect", "--expect is required", "--verdict", "dismiss", "--rationale", "r"),
			Entry("malformed --retire-verdict", "<job>:<source>:<target>", "--verdict", "retire", "--rationale", "r", "--expect", "t", "--retire-verdict", "a:b"),
			Entry("empty part in --retire-verdict", "--retire-verdict", "--verdict", "retire", "--rationale", "r", "--expect", "t", "--retire-verdict", "a::c"),
			Entry("--retire-verdict with dismiss", "only valid with --verdict retire", "--verdict", "dismiss", "--rationale", "r", "--expect", "t", "--retire-verdict", "a:b:c"),
		)
	})

	Describe("help", func() {
		DescribeTable("states the flow and the reviewer-role limit",
			func(want string, args ...string) {
				out, err := run(append(args, "--help")...)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(ContainSubstring(want))
			},
			Entry("run: preview then apply", "preview, then apply with the preview's token", "run"),
			Entry("show: show then decide", "Flow: show, then decide", "show"),
			Entry("decide: reviewer role", "reviewer role, which cannot yet be\ngranted (#232)", "decide"),
			Entry("decide: a stale finding's retire takes every live verdict of the job",
				"retire without --retire-verdict removes every live verdict", "decide"),
			Entry("decide: a stale finding's retire is not capped", "uncapped by max_retire_per_run", "decide"),
			Entry("findings: next page", "exact command for the next\npage", "findings"),
		)
	})
})
