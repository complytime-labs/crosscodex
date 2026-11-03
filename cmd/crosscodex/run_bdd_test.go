package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeStartAnalysisClient records StartAnalysis requests; other
// GatewayServiceClient methods panic through the nil embed.
type fakeStartAnalysisClient struct {
	crosscodexv1connect.GatewayServiceClient
	calls []*pb.StartAnalysisRequest
	err   error
}

func (f *fakeStartAnalysisClient) StartAnalysis(_ context.Context, req *connectrpc.Request[pb.StartAnalysisRequest]) (*connectrpc.Response[pb.StartAnalysisResponse], error) {
	f.calls = append(f.calls, req.Msg)
	if f.err != nil {
		return nil, f.err
	}
	return connectrpc.NewResponse(&pb.StartAnalysisResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
}

var _ = Describe("run start", func() {
	var fake *fakeStartAnalysisClient

	newCmd := func() *cobra.Command {
		cmd := newRunCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		return cmd
	}

	BeforeEach(func() { fake = &fakeStartAnalysisClient{} })

	It("starts analysis of the named catalog and prints the job", func() {
		out, _, err := execute(newCmd(), "start", "--source", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(1))
		Expect(fake.calls[0].GetSourceCatalogId()).To(Equal("cat-1"))
		Expect(fake.calls[0].GetTenantContext()).To(BeNil())
		Expect(out).To(ContainSubstring("Job ID: job-1"))
		Expect(out).To(ContainSubstring("Status: " + formatJobStatus(pb.JobStatus_JOB_STATUS_PENDING)))
	})

	It("passes --tenant as the request's tenant context", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--tenant", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetTenantContext().GetTenantId()).To(Equal("acme"))
	})

	It("emits job_id and status as JSON", func() {
		out, _, err := execute(newCmd(), "start", "--source", "cat-1", "--json")
		Expect(err).NotTo(HaveOccurred())
		var got map[string]string
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]string{"job_id": "job-1", "status": "JOB_STATUS_PENDING"}))
	})

	It("requires --source and says where to find catalog IDs", func() {
		_, _, err := execute(newCmd(), "start")
		Expect(err).To(MatchError(And(ContainSubstring("--source"), ContainSubstring("crosscodex catalog list"))))
		Expect(fake.calls).To(BeEmpty())
	})

	It("refuses a file argument and points at catalog import", func() {
		_, _, err := execute(newCmd(), "start", "nist.json")
		Expect(err).To(MatchError(ContainSubstring("crosscodex catalog import <file>")))
		Expect(fake.calls).To(BeEmpty())
	})

	It("wraps an RPC failure and preserves its code", func() {
		fake.err = connectrpc.NewError(connectrpc.CodeNotFound, errors.New("catalog \"cat-x\" not found"))
		_, _, err := execute(newCmd(), "start", "--source", "cat-x")
		Expect(err).To(MatchError(And(ContainSubstring("failed to start analysis"), ContainSubstring("not found"))))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
		Expect(fake.calls).To(HaveLen(1))
	})
})

// fakeCancelJobClient records CancelJob requests; other GatewayServiceClient
// methods panic through the nil embed.
type fakeCancelJobClient struct {
	crosscodexv1connect.GatewayServiceClient
	calls []*pb.CancelJobRequest
}

func (f *fakeCancelJobClient) CancelJob(_ context.Context, req *connectrpc.Request[pb.CancelJobRequest]) (*connectrpc.Response[pb.CancelJobResponse], error) {
	f.calls = append(f.calls, req.Msg)
	return connectrpc.NewResponse(&pb.CancelJobResponse{}), nil
}

var _ = Describe("confirming commands", func() {
	var fake *fakeCancelJobClient

	// executeWithStdin runs a run subcommand with stdin as its input and the
	// terminal check answering terminal.
	executeWithStdin := func(stdin string, terminal bool, args ...string) (string, error) {
		original := stdinIsTerminal
		stdinIsTerminal = func(io.Reader) bool { return terminal }
		DeferCleanup(func() { stdinIsTerminal = original })

		cmd := newRunCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		cmd.SetIn(strings.NewReader(stdin))
		out, _, err := execute(cmd, args...)
		return out, err
	}

	BeforeEach(func() { fake = &fakeCancelJobClient{} })

	DescribeTable("refuse without --yes when stdin is not a terminal, without prompting or acting",
		func(args []string, action string) {
			// The piped "y" proves the answer is never read: a script cannot
			// confirm on a person's behalf.
			out, err := executeWithStdin("y\n", false, args...)
			Expect(err).To(MatchError(And(
				ContainSubstring("refusing to "+action+" without confirmation"),
				ContainSubstring("stdin is not a terminal"),
				ContainSubstring("pass --yes to confirm"),
			)))
			Expect(out).NotTo(ContainSubstring("(y/N)"))
			Expect(fake.calls).To(BeEmpty())
		},
		Entry("run cancel", []string{"cancel", "job-1"}, "cancel job job-1"),
		Entry("run stop (alias of cancel)", []string{"stop", "job-1"}, "cancel job job-1"),
	)

	DescribeTable("act without prompting when --yes is given, whether or not stdin is a terminal",
		func(args []string, terminal bool) {
			out, err := executeWithStdin("", terminal, args...)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("(y/N)"))
			Expect(fake.calls).To(HaveLen(1))
			Expect(fake.calls[0].GetJobId()).To(Equal("job-1"))
		},
		Entry("run cancel --yes, no terminal", []string{"cancel", "--yes", "job-1"}, false),
		Entry("run cancel -y, terminal", []string{"cancel", "-y", "job-1"}, true),
	)

	DescribeTable("on a terminal, prompt and act only on a yes answer",
		func(answer string, wantCalls int) {
			out, err := executeWithStdin(answer, true, "cancel", "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Cancel job job-1? (y/N): "))
			Expect(fake.calls).To(HaveLen(wantCalls))
		},
		Entry("y cancels", "y\n", 1),
		Entry("YES cancels", "YES\n", 1),
		Entry("n does not cancel", "n\n", 0),
		Entry("an empty line does not cancel", "\n", 0),
	)
})

var _ = Describe("stdinIsTerminal", func() {
	DescribeTable("reports no terminal for input a person cannot type into",
		func(in func() io.Reader) {
			Expect(stdinIsTerminal(in())).To(BeFalse())
		},
		Entry("an in-memory reader", func() io.Reader { return strings.NewReader("y\n") }),
		Entry("a regular file", func() io.Reader {
			f, err := os.CreateTemp(GinkgoT().TempDir(), "stdin")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(f.Close)
			return f
		}),
	)
})

// fakeGetJobClient serves GetJob with job; other GatewayServiceClient methods
// panic through the nil embed.
type fakeGetJobClient struct {
	crosscodexv1connect.GatewayServiceClient
	job *pb.PipelineJob
}

func (f *fakeGetJobClient) GetJob(context.Context, *connectrpc.Request[pb.GetJobRequest]) (*connectrpc.Response[pb.GetJobResponse], error) {
	return connectrpc.NewResponse(&pb.GetJobResponse{Job: f.job}), nil
}

var _ = Describe("run status", func() {
	// jobWithStages is a completed job whose stages are named stageNames.
	jobWithStages := func(stageNames ...string) *pb.PipelineJob {
		job := &pb.PipelineJob{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_COMPLETED}
		for _, name := range stageNames {
			job.Stages = append(job.Stages, &pb.JobStage{StageName: name, Status: pb.JobStatus_JOB_STATUS_COMPLETED})
		}
		return job
	}
	runStatus := func(job *pb.PipelineJob, args ...string) string {
		cmd := newRunCmd(&cliState{client: &fakeGetJobClient{job: job}})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		out, _, err := execute(cmd, append([]string{"status", "job-1"}, args...)...)
		Expect(err).NotTo(HaveOccurred())
		return out
	}
	const noMappingsNote = "this job has no relationship analyzer, so it produces no mappings"

	DescribeTable("lists the analyzers the job ran, in stage order",
		func(stageNames []string, want string) {
			Expect(runStatus(jobWithStages(stageNames...))).To(ContainSubstring("Analyzers: " + want + "\n"))
		},
		Entry("pair analyzers", []string{"classify", "embedding", "candidate_generation", "relationship", "requires", "synthesis", "graph"},
			"classify, embedding, relationship, requires"),
		Entry("no pair analyzer", []string{"classify", "embedding", "synthesis", "graph"}, "classify, embedding"),
		Entry("no analyzer", []string{"synthesis", "graph"}, "none"),
	)

	DescribeTable("notes that a job without the relationship analyzer produces no mappings, and how to enable it",
		func(stageNames []string) {
			out := runStatus(jobWithStages(stageNames...))
			Expect(out).To(ContainSubstring(noMappingsNote))
			Expect(out).To(ContainSubstring("set analysis.relationship.enabled"))
			Expect(out).To(ContainSubstring("then start a new job"))
		},
		Entry("only per-control analyzers", []string{"classify", "embedding", "artifacts", "synthesis", "graph"}),
		Entry("requires only: REQUIRES edges are not mappings", []string{"classify", "embedding", "candidate_generation", "requires", "synthesis", "graph"}),
		Entry("no analyzer", []string{"synthesis", "graph"}),
	)

	It("shows how many stages completed, failed and were cancelled", func() {
		job := jobWithStages("classify", "embedding", "synthesis", "graph")
		job.Progress = &pb.JobProgress{TotalSteps: 4, CompletedSteps: 1, FailedSteps: 1, CancelledSteps: 1}
		Expect(runStatus(job)).To(ContainSubstring("Stages: 1 of 4 completed, 1 failed, 1 cancelled\n"))
	})

	It("gives no note when the relationship analyzer ran", func() {
		Expect(runStatus(jobWithStages("classify", "embedding", "candidate_generation", "relationship", "synthesis", "graph"))).
			NotTo(ContainSubstring(noMappingsNote))
	})

	It("adds the analyzers to the JSON output", func() {
		out := runStatus(jobWithStages("classify", "embedding", "synthesis", "graph"), "--json")
		var got map[string]any
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("analyzers", []any{"classify", "embedding"}))
		Expect(got).To(HaveKeyWithValue("job_id", "job-1"))
	})

	It("reports an empty analyzer list as an empty JSON array", func() {
		out := runStatus(jobWithStages("synthesis", "graph"), "--json")
		var got map[string]any
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("analyzers", []any{}))
	})
})

// fakeRetryJobClient records RetryJob requests and answers with resp or
// err; other GatewayServiceClient methods panic through the nil embed.
type fakeRetryJobClient struct {
	crosscodexv1connect.GatewayServiceClient
	resp  *pb.RetryJobResponse
	err   error
	calls []*pb.RetryJobRequest
}

func (f *fakeRetryJobClient) RetryJob(_ context.Context, req *connectrpc.Request[pb.RetryJobRequest]) (*connectrpc.Response[pb.RetryJobResponse], error) {
	f.calls = append(f.calls, req.Msg)
	if f.err != nil {
		return nil, f.err
	}
	return connectrpc.NewResponse(f.resp), nil
}

var _ = Describe("run retry", func() {
	var fake *fakeRetryJobClient

	run := func(args ...string) (string, error) {
		cmd := newRunCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		out, _, err := execute(cmd, append([]string{"retry"}, args...)...)
		return out, err
	}

	It("resumes the job in place from its failure with --from-failure", func() {
		fake = &fakeRetryJobClient{resp: &pb.RetryJobResponse{NewJobId: "job-1"}}
		out, err := run("--from-failure", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(1))
		Expect(fake.calls[0].GetJobId()).To(Equal("job-1"))
		Expect(fake.calls[0].GetRetryFromFailure()).To(BeTrue())
		Expect(out).To(And(ContainSubstring("job-1"), ContainSubstring("resumed from its failure"), ContainSubstring("crosscodex run status job-1")))
	})

	It("starts a new job with the same config without --from-failure, naming it", func() {
		fake = &fakeRetryJobClient{resp: &pb.RetryJobResponse{NewJobId: "job-2"}}
		out, err := run("--json", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetRetryFromFailure()).To(BeFalse())
		var got map[string]any
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]any{"job_id": "job-2", "retried_job_id": "job-1", "from_failure": false}))
	})

	It("reports the daemon's refusal with its reason", func() {
		fake = &fakeRetryJobClient{err: connectrpc.NewError(connectrpc.CodeFailedPrecondition,
			errors.New("cannot retry job job-1 from its failure: its attestation does not verify. Start a new job instead"))}
		_, err := run("--from-failure", "job-1")
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeFailedPrecondition))
		Expect(err).To(MatchError(And(ContainSubstring("failed to retry job job-1"), ContainSubstring("Start a new job instead"))))
	})

	It("requires a job ID, retrying nothing", func() {
		fake = &fakeRetryJobClient{}
		_, err := run("--from-failure")
		Expect(err).To(MatchError(ContainSubstring("job-id")))
		Expect(fake.calls).To(BeEmpty())
	})
})
