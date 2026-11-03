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
		out, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(1))
		Expect(fake.calls[0].GetSourceCatalogId()).To(Equal("cat-1"))
		Expect(fake.calls[0].GetTargetCatalogId()).To(Equal("cat-2"))
		Expect(fake.calls[0].GetBidirectional()).To(BeFalse())
		Expect(fake.calls[0].GetTenantContext()).To(BeNil())
		Expect(out).To(ContainSubstring("Job ID: job-1"))
		Expect(out).To(ContainSubstring("Status: " + formatJobStatus(pb.JobStatus_JOB_STATUS_PENDING)))
	})

	It("passes --tenant as the request's tenant context", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2", "--tenant", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetTenantContext().GetTenantId()).To(Equal("acme"))
	})

	It("emits job_id and status as JSON", func() {
		out, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2", "--json")
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
		_, _, err := execute(newCmd(), "start", "--source", "cat-x", "--target", "cat-2")
		Expect(err).To(MatchError(And(ContainSubstring("failed to start analysis"), ContainSubstring("not found"))))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
		Expect(fake.calls).To(HaveLen(1))
	})

	It("passes --bidirectional", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2", "--bidirectional")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetBidirectional()).To(BeTrue())
	})

	It("analyses within one catalog when --target equals --source", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetSourceCatalogId()).To(Equal("cat-1"))
		Expect(fake.calls[0].GetTargetCatalogId()).To(Equal("cat-1"))
	})

	It("passes --no-cache", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2", "--no-cache")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetNoCache()).To(BeTrue())
	})

	It("leaves the cache on without --no-cache", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetNoCache()).To(BeFalse())
	})

	It("requires --target, says both forms, and never calls the server", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1")
		Expect(err).To(MatchError(SatisfyAll(
			ContainSubstring("requires --target <catalog-id>"),
			ContainSubstring("--source cat-1 --target cat-1"),
			ContainSubstring("crosscodex catalog list"),
		)))
		Expect(fake.calls).To(BeEmpty())
	})

	It("refuses --bidirectional within one catalog and never calls the server", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--target", "cat-1", "--bidirectional")
		Expect(err).To(MatchError(SatisfyAll(
			ContainSubstring("--bidirectional"),
			ContainSubstring("already pairs its controls in both directions"),
			ContainSubstring("drop --bidirectional, or pass a different --target"),
		)))
		Expect(fake.calls).To(BeEmpty())
	})

	It("shows a crosswalk and the within-catalog form in its help", func() {
		out, _, err := execute(newCmd(), "start", "--help")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(SatisfyAll(
			ContainSubstring("crosscodex run start --source 3f2a9c1e --target 7b41d0aa"),
			ContainSubstring("crosscodex run start --source 3f2a9c1e --target 3f2a9c1e"),
			ContainSubstring("--bidirectional"),
		))
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

// fakeGetJobClient serves GetJob with job (jobErr when set) and GetCatalog
// from catalogs (NotFound for an absent ID, catalogErr for every lookup when
// set),
// recording each catalog lookup; other GatewayServiceClient methods panic
// through the nil embed.
type fakeGetJobClient struct {
	crosscodexv1connect.GatewayServiceClient
	job            *pb.PipelineJob
	jobErr         error
	catalogs       map[string]*pb.Catalog
	catalogErr     error
	catalogLookups []string
	// jobRequests records each GetJob request.
	jobRequests []*pb.GetJobRequest
}

func (f *fakeGetJobClient) GetJob(_ context.Context, req *connectrpc.Request[pb.GetJobRequest]) (*connectrpc.Response[pb.GetJobResponse], error) {
	f.jobRequests = append(f.jobRequests, req.Msg)
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	return connectrpc.NewResponse(&pb.GetJobResponse{Job: f.job}), nil
}

func (f *fakeGetJobClient) GetCatalog(_ context.Context, req *connectrpc.Request[pb.GetCatalogRequest]) (*connectrpc.Response[pb.GetCatalogResponse], error) {
	f.catalogLookups = append(f.catalogLookups, req.Msg.GetCatalogId())
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	c, ok := f.catalogs[req.Msg.GetCatalogId()]
	if !ok {
		return nil, connectrpc.NewError(connectrpc.CodeNotFound, errors.New("catalog not found. List catalogs to find a valid catalog_id"))
	}
	return connectrpc.NewResponse(&pb.GetCatalogResponse{Catalog: c}), nil
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

	It("prints one result cache line per analyzer, aligned, and the summaries in JSON", func() {
		job := jobWithStages("classify", "embedding", "synthesis", "graph")
		job.CacheSummaries = []*pb.AnalyzerCacheSummary{
			{Analyzer: "classify", Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED,
				Mode: pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED, Tasks: 2, Hits: 2,
				Models: []*pb.AnalyzerCacheModel{{Alias: "default", ObservedModel: "qwen2.5:14b"}}},
			{Analyzer: "embedding", Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED},
		}
		Expect(runStatus(job)).To(ContainSubstring("Result cache:\n" +
			"  classify   2 of 2 reused, 0 sent to model default → qwen2.5:14b\n" +
			"  embedding  cache use not attested: no signed checkpoint vouches for this stage's records\n"))

		var got map[string]any
		Expect(json.Unmarshal([]byte(runStatus(job, "--json")), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("cache", HaveLen(2)))
	})

	It("asks the daemon for the job's cache summaries", func() {
		client := &fakeGetJobClient{job: jobWithStages("classify", "synthesis", "graph")}
		cmd := newRunCmd(&cliState{client: client})
		cmd.PersistentFlags().Bool("json", false, "output as JSON")
		_, _, err := execute(cmd, "status", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(client.jobRequests).To(HaveLen(1))
		Expect(client.jobRequests[0].GetIncludeCacheSummaries()).To(BeTrue())
	})

	It("prints no result cache section for a job without summaries, and an empty JSON list", func() {
		job := jobWithStages("classify", "synthesis", "graph")
		Expect(runStatus(job)).NotTo(ContainSubstring("Result cache"))
		var got map[string]any
		Expect(json.Unmarshal([]byte(runStatus(job, "--json")), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("cache", []any{}))
	})

	It("says in embedded mode that cache use is shown only by the daemon", func() {
		cmd := newRunCmd(&cliState{client: &fakeGetJobClient{job: jobWithStages("classify", "synthesis", "graph")}, daemon: &embeddedDaemon{}})
		cmd.PersistentFlags().Bool("json", false, "output as JSON")
		out, _, err := execute(cmd, "status", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Result cache: shown only when connected to crosscodexd\n"))
	})

	It("reports an empty analyzer list as an empty JSON array", func() {
		out := runStatus(jobWithStages("synthesis", "graph"), "--json")
		var got map[string]any
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(HaveKeyWithValue("analyzers", []any{}))
	})

	Describe("the job's catalogs", func() {
		catalogs := map[string]*pb.Catalog{
			"cat-src": {CatalogId: "cat-src", Name: "CSF 2.0", Version: "2.0", ControlCount: 185},
			"cat-tgt": {CatalogId: "cat-tgt", Name: "NIST SP 800-53", ControlCount: 1196},
		}
		statusOf := func(client *fakeGetJobClient, args ...string) (string, error) {
			cmd := newRunCmd(&cliState{client: client})
			cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
			out, _, err := execute(cmd, append([]string{"status", "job-1"}, args...)...)
			return out, err
		}
		jobOver := func(cfg *pb.JobConfig) *pb.PipelineJob {
			job := jobWithStages("classify", "synthesis", "graph")
			job.Config = cfg
			return job
		}
		// recorded gives cfg the snapshots CreateJob records: each side's
		// label from catalogs, as they were when the job was created.
		recorded := func(cfg *pb.JobConfig) *pb.JobConfig {
			snap := func(id string) *pb.CatalogSnapshot {
				c := catalogs[id]
				return &pb.CatalogSnapshot{CatalogId: id, Name: c.GetName(), Version: c.GetVersion()}
			}
			target := cfg.GetTargetCatalogId()
			if target == "" {
				target = cfg.GetCatalogId()
			}
			cfg.SourceCatalog, cfg.TargetCatalog = snap(cfg.GetCatalogId()), snap(target)
			return cfg
		}
		const notRecorded = " (current name; not recorded when the job was created)"

		It("shows source and target by name, version and ID, each side's control count, and the direction", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt", Bidirectional: true}))}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(SatisfyAll(
				ContainSubstring("Source catalog: CSF 2.0 (version 2.0, ID cat-src, 185 controls)\n"),
				ContainSubstring("Target catalog: NIST SP 800-53 (version not recorded, ID cat-tgt, 1196 controls)\n"),
				ContainSubstring("Direction: both ways (source to target, and target to source)\n"),
			))
			Expect(client.catalogLookups).To(Equal([]string{"cat-src", "cat-tgt"}))
		})

		It("shows a one-way crosswalk's direction", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"}))}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Direction: source to target\n"))
		})

		DescribeTable("shows a job within one catalog with that catalog on both sides, looked up once",
			func(cfg *pb.JobConfig) {
				client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(cfg)}
				out, err := statusOf(client)
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(SatisfyAll(
					ContainSubstring("Source catalog: CSF 2.0 (version 2.0, ID cat-src, 185 controls)\n"),
					ContainSubstring("Target catalog: CSF 2.0 (version 2.0, ID cat-src, 185 controls)\n"),
					ContainSubstring("Direction: within one catalog (its controls paired in both directions)\n"),
				))
				Expect(client.catalogLookups).To(Equal([]string{"cat-src"}))
			},
			Entry("explicit target equal to the source", recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-src"})),
			Entry("a target-less config with snapshots", recorded(&pb.JobConfig{CatalogId: "cat-src"})),
		)

		// A re-import under a new name overwrites the catalog's name but
		// keeps its ID, so the job's own record is what names its catalogs.
		It("shows a renamed catalog under the name and version the job recorded", func() {
			job := jobOver(recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"}))
			renamed := map[string]*pb.Catalog{
				"cat-src": {CatalogId: "cat-src", Name: "CSF 2.0 (renamed)", Version: "2.0.1", ControlCount: 185},
				"cat-tgt": catalogs["cat-tgt"],
			}
			out, err := statusOf(&fakeGetJobClient{catalogs: renamed, job: job})
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Source catalog: CSF 2.0 (version 2.0, ID cat-src, 185 controls)\n"))
			Expect(out).NotTo(ContainSubstring("renamed"))
		})

		It("still names a catalog deleted since the job by the label the job recorded", func() {
			cfg := recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"})
			client := &fakeGetJobClient{catalogs: map[string]*pb.Catalog{"cat-src": catalogs["cat-src"]}, job: jobOver(cfg)}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Target catalog: NIST SP 800-53 (version not recorded, ID cat-tgt, " +
				"not found: deleted or purged, or not visible to your tenant)\n"))
		})

		It("labels a job from before snapshots as showing the catalogs' current names", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"})}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(SatisfyAll(
				ContainSubstring("Source catalog: CSF 2.0 (version 2.0, ID cat-src, 185 controls)"+notRecorded+"\n"),
				ContainSubstring("Target catalog: NIST SP 800-53 (version not recorded, ID cat-tgt, 1196 controls)"+notRecorded+"\n"),
			))
		})

		It("marks a job from before snapshots in the JSON output", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(&pb.JobConfig{CatalogId: "cat-src"})}
			out, err := statusOf(client, "--json")
			Expect(err).NotTo(HaveOccurred())
			var got struct {
				Analysis struct {
					Source map[string]any `json:"source_catalog"`
					Target map[string]any `json:"target_catalog"`
				} `json:"analysis"`
			}
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got.Analysis.Source).To(SatisfyAll(HaveKeyWithValue("name", "CSF 2.0"), HaveKeyWithValue("snapshot", false)))
			Expect(got.Analysis.Target).To(HaveKeyWithValue("snapshot", false))
		})

		It("still shows a job whose catalog is gone, by ID", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-gone"})}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Target catalog: cat-gone (not found: deleted or purged, or not visible to your tenant)\n"))
		})

		It("shows an uploader-controlled name and version as one clean line", func() {
			client := &fakeGetJobClient{job: jobOver(&pb.JobConfig{CatalogId: "cat-evil", TargetCatalogId: "cat-evil",
				SourceCatalog: &pb.CatalogSnapshot{CatalogId: "cat-evil", Name: "Evil\x1b]0;pwned\x07\nCatalog", Version: "1\u202e0"}}),
				catalogs: map[string]*pb.Catalog{"cat-evil": {CatalogId: "cat-evil", Name: "Current", ControlCount: 1}}}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Source catalog: Evil ]0;pwned Catalog (version 10, ID cat-evil, 1 controls)\n"))
			Expect(out).NotTo(ContainSubstring("\x1b"))
		})

		// Jobs created before CreateJob confirmed catalog ownership can name
		// any string as a catalog ID.
		It("shows an unvalidated catalog ID as one clean line, found or not", func() {
			evilID := "cat\x1b[2J\nsrc"
			client := &fakeGetJobClient{job: jobOver(&pb.JobConfig{CatalogId: evilID, TargetCatalogId: "gone\x1b[2J"}),
				catalogs: map[string]*pb.Catalog{evilID: {CatalogId: evilID, Name: "CSF 2.0", Version: "2.0", ControlCount: 1}}}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(SatisfyAll(
				ContainSubstring("Source catalog: CSF 2.0 (version 2.0, ID cat [2J src, 1 controls)"+notRecorded+"\n"),
				ContainSubstring("Target catalog: gone [2J (not found: deleted or purged, or not visible to your tenant)\n"),
			))
			Expect(out).NotTo(ContainSubstring("\x1b"))
		})

		It("still shows the job when a catalog lookup fails, marking each catalog it could not read", func() {
			client := &fakeGetJobClient{catalogErr: connectrpc.NewError(connectrpc.CodeUnavailable, errors.New("catalog backend down\x1b[2J")),
				job: jobOver(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"})}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(SatisfyAll(
				ContainSubstring("Job: job-1\n"),
				ContainSubstring("Status: "),
				ContainSubstring("Source catalog: cat-src (lookup failed: unavailable: catalog backend down [2J)\n"),
				ContainSubstring("Target catalog: cat-tgt (lookup failed: unavailable: catalog backend down [2J)\n"),
				ContainSubstring("Direction: source to target\n"),
			))
			Expect(out).NotTo(ContainSubstring("\x1b"))
			Expect(client.catalogLookups).To(Equal([]string{"cat-src", "cat-tgt"}))
		})

		It("reports a failed catalog lookup in the JSON output", func() {
			client := &fakeGetJobClient{catalogErr: connectrpc.NewError(connectrpc.CodeUnavailable, errors.New("catalog backend down")),
				job: jobOver(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"})}
			out, err := statusOf(client, "--json")
			Expect(err).NotTo(HaveOccurred())
			var got struct {
				Analysis struct {
					Source map[string]any `json:"source_catalog"`
				} `json:"analysis"`
			}
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got.Analysis.Source).To(SatisfyAll(
				HaveKeyWithValue("catalog_id", "cat-src"), HaveKeyWithValue("found", false),
				HaveKeyWithValue("lookup_error", "unavailable: catalog backend down"),
			))
		})

		It("fails when the job itself cannot be read", func() {
			client := &fakeGetJobClient{jobErr: connectrpc.NewError(connectrpc.CodeUnavailable, errors.New("gateway down"))}
			_, err := statusOf(client)
			Expect(err).To(MatchError(SatisfyAll(ContainSubstring("failed to get job status"), ContainSubstring("gateway down"))))
			Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeUnavailable))
			Expect(client.catalogLookups).To(BeEmpty())
		})

		It("adds the catalogs and direction to the JSON output", func() {
			client := &fakeGetJobClient{catalogs: catalogs, job: jobOver(recorded(&pb.JobConfig{CatalogId: "cat-src", TargetCatalogId: "cat-tgt"}))}
			out, err := statusOf(client, "--json")
			Expect(err).NotTo(HaveOccurred())
			var got struct {
				Analysis struct {
					Source    map[string]any `json:"source_catalog"`
					Target    map[string]any `json:"target_catalog"`
					Direction string         `json:"direction"`
				} `json:"analysis"`
			}
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got.Analysis.Direction).To(Equal("source_to_target"))
			Expect(got.Analysis.Source).To(SatisfyAll(
				HaveKeyWithValue("catalog_id", "cat-src"), HaveKeyWithValue("name", "CSF 2.0"),
				HaveKeyWithValue("version", "2.0"), HaveKeyWithValue("control_count", BeNumerically("==", 185)),
				HaveKeyWithValue("found", true), HaveKeyWithValue("snapshot", true),
			))
			Expect(got.Analysis.Target).To(HaveKeyWithValue("catalog_id", "cat-tgt"))
		})

		It("shows no catalogs for a job whose config names none, and looks none up", func() {
			client := &fakeGetJobClient{job: jobWithStages("synthesis", "graph")}
			out, err := statusOf(client)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("catalog:"))
			Expect(client.catalogLookups).To(BeEmpty())
		})
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
