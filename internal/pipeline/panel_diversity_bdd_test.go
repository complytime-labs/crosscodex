package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// panelStub is a per-control analyzer that declares a panel.
type panelStub struct {
	*analyzertest.Stub[*pb.Control]
	panel analyzer.Panel
}

func (s *panelStub) Panel() analyzer.Panel { return s.panel }

var _ = Describe("Panel diversity check", func() {
	var (
		ctx    context.Context
		models map[string]string // alias -> reported model
		deploy map[string]string // alias -> reported deployment ID
		llm    *llmclienttest.Fake
		logs   *bytes.Buffer
	)
	panel := []string{"panel-1", "panel-2", "panel-3"}
	unknownMsg := func(stage, alias string) string {
		return `stage "` + stage + `" stopped: the upstream model behind panel alias ` + alias + ` is unknown: the gateway did not report ` +
			`the model behind the alias (a LiteLLM gateway exposes it at /v1/model/info), so panel independence cannot be verified. ` +
			`Make the gateway report the model behind each alias, or set analysis.` + stage + `.allow_shared_panel_models: true to run without the check.`
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
		models = map[string]string{"panel-1": "a", "panel-2": "b", "panel-3": "c"}
		deploy = map[string]string{}
		llm = &llmclienttest.Fake{
			CompleteFunc: func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return &llmclient.CompletionResponse{Model: models[req.Model], Deployment: deploy[req.Model]}, nil
			},
		}
		logs = &bytes.Buffer{}
	})

	service := func(opts ...pipeline.Option) *pipeline.Service {
		opts = append([]pipeline.Option{pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))}, opts...)
		return pipeline.New(newFakeStore(), nil, analyzer.NewRegistry(), nil, pipeline.Attestors{}, nil, nil, &storagetest.Fake{},
			config.PipelineConfig{}, nil, opts...)
	}

	DescribeTable("checks one stage's panel",
		func(set func(), stage string, allowShared bool, wantErr string) {
			set()
			svc := service(pipeline.WithPanelProbe(llm))
			got, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", stage, panel, allowShared)
			if wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(HaveLen(3))
				return
			}
			Expect(err).To(MatchError(pipeline.ErrPanelNotIndependent))
			Expect(err.Error()).To(Equal(wantErr))
		},
		Entry("distinct models pass", func() {}, "relationship", false, ""),
		Entry("two aliases on one model are refused", func() { models["panel-3"] = "m"; models["panel-1"] = "m" }, "relationship", false,
			`stage "relationship" stopped: panel aliases panel-1 and panel-3 both answered as m, so their votes are not independent `+
				`and confidence would only measure one model's self-consistency. Point the aliases at different models in the LLM `+
				`gateway config, or set analysis.relationship.allow_shared_panel_models: true to run a single-model panel.`),
		Entry("the requires stage names its own setting", func() { models["panel-2"] = "a" }, "requires", false,
			`stage "requires" stopped: panel aliases panel-1 and panel-2 both answered as a, so their votes are not independent `+
				`and confidence would only measure one model's self-consistency. Point the aliases at different models in the LLM `+
				`gateway config, or set analysis.requires.allow_shared_panel_models: true to run a single-model panel.`),
		Entry("an alias echoed back with no deployment is an unknown model", func() { models["panel-2"] = "panel-2" }, "relationship", false,
			unknownMsg("relationship", "panel-2")),
		Entry("an empty model name is unknown", func() { models["panel-1"] = "" }, "critique", false,
			unknownMsg("critique", "panel-1")),
		Entry("an unprintable model name is unknown", func() { models["panel-1"] = "\x1b[2Jm" }, "relationship", false,
			unknownMsg("relationship", "panel-1")),
		Entry("a shared model passes when allowed", func() { models["panel-3"] = "a" }, "relationship", true, ""),
		Entry("an unknown model passes when allowed", func() { models["panel-3"] = "" }, "relationship", true, ""),
	)

	Describe("reading the upstream model of an alias the gateway only echoes", func() {
		catalogOf := func(entries ...llmclient.ModelDeployment) {
			llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) { return entries, nil }
		}
		echoAll := func() {
			for _, a := range panel {
				models[a] = a
				deploy[a] = "dep-" + a
			}
		}
		deployment := func(alias, model string) llmclient.ModelDeployment {
			return llmclient.ModelDeployment{Alias: alias, ID: "dep-" + alias, Model: model}
		}
		sharedMsg := func(a, b, model string) string {
			return `stage "relationship" stopped: panel aliases ` + a + ` and ` + b + ` both answered as ` + model + `, so their votes are not independent ` +
				`and confidence would only measure one model's self-consistency. Point the aliases at different models in the LLM ` +
				`gateway config, or set analysis.relationship.allow_shared_panel_models: true to run a single-model panel.`
		}

		DescribeTable("checks the panel",
			func(set func(), allowShared bool, wantErr error, wantMsg string) {
				echoAll()
				set()
				svc := service(pipeline.WithPanelProbe(llm))
				got, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, allowShared)
				if wantErr == nil {
					Expect(err).NotTo(HaveOccurred())
					Expect(got).To(HaveLen(3))
					return
				}
				Expect(err).To(MatchError(wantErr))
				Expect(err.Error()).To(Equal(wantMsg))
			},
			Entry("distinct catalog models pass", func() {
				catalogOf(deployment("panel-1", "openai/a"), deployment("panel-2", "openai/b"), deployment("panel-3", "openai/c"))
			}, false, nil, ""),
			Entry("one catalog model behind two aliases is refused, naming both aliases and the model", func() {
				catalogOf(deployment("panel-1", "openai/a"), deployment("panel-2", "openai/b"), deployment("panel-3", "openai/a"))
			}, false, pipeline.ErrPanelNotIndependent, sharedMsg("panel-1", "panel-3", "openai/a")),
			Entry("models differing only in case are shared", func() {
				catalogOf(deployment("panel-1", "openai/Qwen"), deployment("panel-2", "openai/b"), deployment("panel-3", "openai/qwen"))
			}, false, pipeline.ErrPanelNotIndependent, sharedMsg("panel-1", "panel-3", "openai/Qwen and openai/qwen (the same model)")),
			Entry("models differing only in provider prefix are shared", func() {
				catalogOf(deployment("panel-1", "openai/m"), deployment("panel-2", "hosted_vllm/m"), deployment("panel-3", "openai/c"))
			}, false, pipeline.ErrPanelNotIndependent, sharedMsg("panel-1", "panel-2", "openai/m and hosted_vllm/m (the same model)")),
			Entry("a model with a nested provider prefix equals the unprefixed model", func() {
				catalogOf(deployment("panel-1", "hosted_vllm/Qwen/Qwen3"), deployment("panel-2", "Qwen/Qwen3"), deployment("panel-3", "openai/c"))
			}, false, pipeline.ErrPanelNotIndependent, sharedMsg("panel-1", "panel-2", "hosted_vllm/Qwen/Qwen3 and Qwen/Qwen3 (the same model)")),
			Entry("models that merely end alike are not shared", func() {
				catalogOf(deployment("panel-1", "openai/a-m"), deployment("panel-2", "openai/m"), deployment("panel-3", "openai/c"))
			}, false, nil, ""),
			Entry("a catalog without the gateway endpoint is an unknown model", func() {
				llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) {
					return nil, llmclient.ErrModelCatalogUnsupported
				}
			}, false, pipeline.ErrPanelNotIndependent, unknownMsg("relationship", "panel-1")),
			Entry("a deployment ID with no matching entry is an unknown model", func() {
				catalogOf(deployment("panel-1", "openai/a"), llmclient.ModelDeployment{Alias: "panel-2", ID: "other", Model: "openai/b"},
					deployment("panel-3", "openai/c"))
			}, false, pipeline.ErrPanelNotIndependent, unknownMsg("relationship", "panel-2")),
			Entry("an entry for another alias with the same ID does not match", func() {
				catalogOf(deployment("panel-1", "openai/a"), llmclient.ModelDeployment{Alias: "other", ID: "dep-panel-2", Model: "openai/b"},
					deployment("panel-3", "openai/c"))
			}, false, pipeline.ErrPanelNotIndependent, unknownMsg("relationship", "panel-2")),
			Entry("an unusable catalog model is unknown", func() {
				catalogOf(deployment("panel-1", "openai/a"), deployment("panel-2", "\x1b[2Jb"), deployment("panel-3", "openai/c"))
			}, false, pipeline.ErrPanelNotIndependent, unknownMsg("relationship", "panel-2")),
			Entry("a gateway key refused by the catalog fails with an actionable message", func() {
				llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) {
					return nil, llmclient.ErrAuthentication
				}
			}, false, pipeline.ErrPanelCatalogDenied,
				`stage "relationship" stopped: the LLM gateway refused the API key's request for its model catalog (/v1/model/info), so the model `+
					`behind each panel alias cannot be read. Let the key read /v1/model/info, or set analysis.relationship.allow_shared_panel_models: `+
					`true to run without the check.`),
			Entry("a shared catalog model passes when allowed", func() {
				catalogOf(deployment("panel-1", "openai/a"), deployment("panel-2", "openai/a"), deployment("panel-3", "openai/c"))
			}, true, nil, ""),
			Entry("an unsupported catalog passes when allowed", func() {
				llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) {
					return nil, llmclient.ErrModelCatalogUnsupported
				}
			}, true, nil, ""),
		)

		DescribeTable("lets a catalog fault through under the allowance, recording the alias as unknown with the fault",
			func(fault error, want string) {
				echoAll()
				llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) { return nil, fault }
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
				svc := service(pipeline.WithPanelProbe(llm), pipeline.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
				ids, err := svc.CheckPanelIdentities(ctx, "test-tenant", "job-1", "relationship", panel, true)
				Expect(err).NotTo(HaveOccurred())
				Expect(ids["panel-2"]).To(Equal(pipeline.PanelIdentity{Source: "unknown", DeploymentID: "dep-panel-2", Fault: want}))
				by := pipeline.PanelByProducts(panel, true, ids)
				Expect(by).To(HaveKeyWithValue("panel.independence", "allowed shared or unknown upstream model"))
				Expect(by).To(HaveKeyWithValue("panel.allow_shared", true))
				m := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.pipeline.panel_checks")
				Expect(m).NotTo(BeNil())
				sum, ok := m.Data.(metricdata.Sum[int64])
				Expect(ok).To(BeTrue())
				outcome, _ := sum.DataPoints[0].Attributes.Value("outcome")
				Expect(outcome.AsString()).To(Equal("allowed"))
			},
			Entry("a refused key", llmclient.ErrAuthentication, "catalog refused"),
			Entry("a failing gateway", llmclient.ErrGatewayUnavailable, "catalog unavailable"),
			Entry("an unreadable catalog", llmclient.ErrInvalidModelCatalog, "catalog unreadable"),
		)

		It("records the identity source and deployment ID of each alias, never a deployment URL", func() {
			models["panel-1"], models["panel-2"], models["panel-3"] = "a", "panel-2", "panel-3"
			deploy["panel-1"] = "dep-1"
			deploy["panel-2"], deploy["panel-3"] = "dep-panel-2", "dep-panel-3"
			catalogOf(deployment("panel-2", "openai/b"))
			ids, err := service(pipeline.WithPanelProbe(llm)).CheckPanelIdentities(ctx, "test-tenant", "job-1", "relationship", panel, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal(map[string]pipeline.PanelIdentity{
				"panel-1": {Model: "a", Source: "reply", DeploymentID: "dep-1"},
				"panel-2": {Model: "openai/b", Source: "catalog", DeploymentID: "dep-panel-2"},
				"panel-3": {Source: "unknown", DeploymentID: "dep-panel-3"},
			}))
		})

		It("wraps a catalog fault that is not a refusal, as an operational failure rather than an unknown model", func() {
			echoAll()
			llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) {
				return nil, llmclient.ErrGatewayUnavailable
			}
			_, err := service(pipeline.WithPanelProbe(llm)).CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
			Expect(err).To(MatchError(llmclient.ErrGatewayUnavailable))
			Expect(err).NotTo(MatchError(pipeline.ErrPanelNotIndependent))
			Expect(err).To(MatchError(ContainSubstring(`reading the model catalog for stage "relationship"`)))
		})

		It("reads the catalog once per check, and not at all when every alias names its model", func() {
			echoAll()
			catalogOf(deployment("panel-1", "openai/a"), deployment("panel-2", "openai/b"), deployment("panel-3", "openai/c"))
			svc := service(pipeline.WithPanelProbe(llm))
			_, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(llm.ModelCatalogCalls()).To(Equal(1))

			named := &llmclienttest.Fake{CompleteFunc: func(_ context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return &llmclient.CompletionResponse{Model: "real-" + req.Model, Deployment: "d-" + req.Model}, nil
			}}
			_, err = service(pipeline.WithPanelProbe(named)).CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(named.ModelCatalogCalls()).To(BeZero())
		})

		It("keeps the gateway's other catalog fields out of the result, the error and the provenance", func() {
			echoAll()
			body := `{"data":[` +
				`{"model_name":"panel-1","litellm_params":{"model":"openai/m","api_base":"http://SECRET-HOST:4000","api_key":"SECRET-KEY"},"model_info":{"id":"dep-panel-1"}},` +
				`{"model_name":"panel-2","litellm_params":{"model":"openai/m","api_base":"http://SECRET-HOST:4000"},"model_info":{"id":"dep-panel-2"}},` +
				`{"model_name":"panel-3","litellm_params":{"model":"openai/c"},"model_info":{"id":"dep-panel-3"}}]}`
			llm.ModelCatalogFunc = func(context.Context) ([]llmclient.ModelDeployment, error) {
				return llmclient.ParseModelCatalog([]byte(body))
			}
			svc := service(pipeline.WithPanelProbe(llm))
			_, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
			Expect(err).To(MatchError(pipeline.ErrPanelNotIndependent))
			Expect(err.Error()).NotTo(Or(ContainSubstring("SECRET"), ContainSubstring("api_base")))

			got, err := svc.CheckPanelIdentities(ctx, "test-tenant", "job-1", "relationship", panel, true)
			Expect(err).NotTo(HaveOccurred())
			by := pipeline.PanelByProducts(panel, true, got)
			Expect(by).To(HaveKeyWithValue("panel.independence", "allowed shared or unknown upstream model"))
			Expect(by).To(HaveKeyWithValue("panel.models", map[string]string{"panel-1": "openai/m", "panel-2": "openai/m", "panel-3": "openai/c"}))
			Expect(by["panel.aliases"]).To(HaveKeyWithValue("panel-1", pipeline.PanelIdentity{Model: "openai/m", Source: "catalog", DeploymentID: "dep-panel-1"}))
			Expect(fmt.Sprint(by)).NotTo(ContainSubstring("SECRET"))
			Expect(logs.String()).NotTo(ContainSubstring("SECRET"))
		})
	})

	It("returns the model each alias reported", func() {
		svc := service(pipeline.WithPanelProbe(llm))
		got, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]string{"panel-1": "a", "panel-2": "b", "panel-3": "c"}))
	})

	It("checks each stage's panel on its own, so stages may reuse aliases", func() {
		svc := service(pipeline.WithPanelProbe(llm))
		for _, stage := range []string{"relationship", "critique"} {
			_, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", stage, panel, false)
			Expect(err).NotTo(HaveOccurred(), stage)
		}
	})

	It("wraps a failed probe and stops after it", func() {
		llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
			return nil, context.DeadlineExceeded
		}
		svc := service(pipeline.WithPanelProbe(llm))
		_, err := svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, false)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err).NotTo(MatchError(pipeline.ErrPanelNotIndependent))
		Expect(err).To(MatchError(ContainSubstring(`panel check: probing alias "panel-1" of stage "relationship"`)))
		Expect(llm.CompleteRequests()).To(HaveLen(1), "no alias is probed after the first failure")
	})

	DescribeTable("counts checks by outcome",
		func(set func(), allowShared bool, probeFails bool, want string) {
			set()
			if probeFails {
				llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
					return nil, context.DeadlineExceeded
				}
			}
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			svc := service(pipeline.WithPanelProbe(llm), pipeline.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
			_, _ = svc.CheckPanelDiversity(ctx, "test-tenant", "job-1", "relationship", panel, allowShared)
			m := telemetrytest.FindMetric(tp.GetMetrics(), "crosscodex.pipeline.panel_checks")
			Expect(m).NotTo(BeNil())
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			Expect(sum.DataPoints).To(HaveLen(1))
			outcome, _ := sum.DataPoints[0].Attributes.Value("outcome")
			Expect(outcome.AsString()).To(Equal(want))
		},
		Entry("independent", func() {}, false, false, "independent"),
		Entry("shared", func() { models["panel-2"] = "a" }, false, false, "shared"),
		Entry("allowed", func() { models["panel-2"] = "a" }, true, false, "allowed"),
		Entry("unknown", func() { models["panel-2"] = "" }, false, false, "unknown"),
		Entry("failed", func() {}, false, true, "failed"),
	)

	DescribeTable("records the panel in the analyzer link's byproducts",
		func(set func(), allowShared bool, want string) {
			set()
			svc := service(pipeline.WithPanelProbe(llm))
			got, err := svc.CheckPanelIdentities(ctx, "test-tenant", "job-1", "relationship", panel, allowShared)
			Expect(err).NotTo(HaveOccurred())
			by := pipeline.PanelByProducts(panel, allowShared, got)
			Expect(by).To(HaveKeyWithValue("panel.independence", want))
			Expect(by["panel.models"]).To(HaveLen(3))
			Expect(by["panel.aliases"]).To(HaveKeyWithValue("panel-1", pipeline.PanelIdentity{Model: models["panel-1"], Source: "reply"}))
		},
		Entry("independent models", func() {}, false, "independent"),
		Entry("independent models with the allowance set", func() {}, true, "independent"),
		Entry("a shared model the allowance let through", func() { models["panel-2"] = "a" }, true, "allowed shared or unknown upstream model"),
		Entry("an unknown model the allowance let through", func() { models["panel-2"] = "" }, true, "allowed shared or unknown upstream model"),
	)

	It("records an unchecked panel when no probe is wired", func() {
		by := pipeline.PanelByProducts(panel, false, nil)
		Expect(by).To(HaveKeyWithValue("panel.independence", "panel independence unchecked"))
	})

	Describe("in a running job", func() {
		var (
			jobs       *fakeStore
			svc        *pipeline.Service
			dispatcher *executorTestDispatcher
		)

		start := func(allowShared bool, opts ...pipeline.Option) string {
			jobs = newFakeStore()
			jobReg := analyzer.NewRegistry()
			inner := cacheableControlStub("classify", analyzer.ModelUse{Alias: "panel-1", Kind: analyzer.ModelCompletion})
			Expect(analyzer.Register[*pb.Control](jobReg, &panelStub{Stub: &inner.Stub,
				panel: analyzer.Panel{Aliases: []string{"panel-1", "panel-2"}, AllowShared: allowShared}})).To(Succeed())
			bus := &natsbustest.Fake{}
			dispatcher = &executorTestDispatcher{}
			engine := analysis.New(jobReg, dispatcher, &executorTestCollector{},
				config.EngineConfig{TaskTimeout: time.Minute, MaxRetries: 1, RetryBackoff: time.Millisecond},
				map[string]natsbus.TaskType{"classify": natsbus.TaskClassify},
				analysis.WithStageReporter(pipeline.NewDBStageReporter(analysis.NewNATSStageReporter(bus), jobs)))
			opts = append([]pipeline.Option{
				pipeline.WithCatalogNameReader(serviceTestCatalogs),
				pipeline.WithCatalogControlsReader(&fakeCatalogControlsReader{controls: []*pb.Control{{ControlId: "cat-1/ac-1", CatalogId: "cat-1"}}}),
				pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
			}, opts...)
			svc = pipeline.New(jobs, engine, jobReg, &fakeSynthesisExecutor{}, pipeline.Attestors{},
				nil, bus, &storagetest.Fake{},
				config.PipelineConfig{MaxConcurrentJobs: 10, StageTimeout: time.Hour}, nil, opts...)
			DeferCleanup(func() { Expect(svc.Stop(context.Background())).To(Succeed()) })
			resp, err := svc.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
				Config: &pb.JobConfig{CatalogId: "cat-1", TargetCatalogId: "cat-1"}}))
			Expect(err).NotTo(HaveOccurred())
			return resp.Msg.JobId
		}
		finished := func(id string) *pipeline.Job {
			var j *pipeline.Job
			Eventually(func() pipeline.JobStatus {
				j, _ = jobs.GetJob(ctx, id)
				if j == nil {
					return ""
				}
				return j.Status
			}).Should(Or(Equal(pipeline.JobStatusCompleted), Equal(pipeline.JobStatusFailed)))
			return j
		}

		It("fails the stage before any analysis call when the panel shares a model", func() {
			models["panel-2"] = "a"
			models["panel-1"] = "a"
			id := start(false, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusFailed))
			stages, err := jobs.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			var msg string
			for _, st := range stages {
				if st.StageName == "classify" {
					msg = st.ErrorMessage
				}
			}
			Expect(msg).To(ContainSubstring("panel aliases panel-1 and panel-2 both answered as a"))
			Expect(dispatcher.dispatchCount()).To(BeZero())
			Expect(llm.CompleteRequests()).To(HaveLen(2), "only the two probes")
		})

		stageOf := func(id, name string) *pipeline.Stage {
			stages, err := jobs.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			for _, st := range stages {
				if st.StageName == name {
					return st
				}
			}
			Fail("no stage " + name)
			return nil
		}

		It("fails the stage through the probe-failure path, with no analysis call, when a probe fails", func() {
			llm.CompleteFunc = func(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				return nil, errors.New("SECRET-DRIVER-DETAIL dial tcp 10.0.0.9:4000")
			}
			id := start(false, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusFailed))
			st := stageOf(id, "classify")
			Expect(st.Status).To(Equal(pipeline.StageStatusFailed))
			Expect(st.ErrorMessage).To(ContainSubstring("did not complete a probe request before analysis"))
			Expect(st.ErrorMessage).NotTo(ContainSubstring("SECRET"))
			Expect(dispatcher.dispatchCount()).To(BeZero())
		})

		It("ends a job cancelled during the panel probe as cancelled, with no stage failure and no analysis call", func() {
			probing := make(chan struct{}, 1)
			llm.CompleteFunc = func(ctx context.Context, _ *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
				select {
				case probing <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			id := start(false, pipeline.WithPanelProbe(llm))
			Eventually(probing).Should(Receive())
			_, err := svc.CancelJob(ctx, connect.NewRequest(&pb.CancelJobRequest{JobId: id}))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() pipeline.JobStatus {
				j, _ := jobs.GetJob(ctx, id)
				return j.Status
			}).Should(Equal(pipeline.JobStatusCancelled))
			Expect(stageOf(id, "classify").Status).NotTo(Equal(pipeline.StageStatusFailed))
			Expect(dispatcher.dispatchCount()).To(BeZero())
		})

		It("stores the panel record with the stage and shows it, independent or not", func() {
			id := start(false, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusCompleted))
			stages, err := jobs.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			got := svc.PanelProvenance(stages)["classify"]
			Expect(got.GetIndependence()).To(Equal("independent"))
			Expect(got.GetAllowShared()).To(BeFalse())
			Expect(got.GetModels()).To(HaveLen(2))
			Expect(got.GetModels()[0].GetAlias()).To(Equal("panel-1"))
			Expect(got.GetModels()[0].GetObservedModel()).To(Equal("a"))
		})

		It("shows an allowed panel with the allowance in effect", func() {
			models["panel-2"], models["panel-1"] = "a", "a"
			id := start(true, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusCompleted))
			stages, err := jobs.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			got := svc.PanelProvenance(stages)["classify"]
			Expect(got.GetIndependence()).To(Equal("allowed shared or unknown upstream model"))
			Expect(got.GetAllowShared()).To(BeTrue())
		})

		It("keeps the record of a completed stage, and says so when a completed stage has none", func() {
			id := start(false, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusCompleted))
			stages, err := jobs.GetStages(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(svc.PanelProvenance(stages)["classify"].GetIndependence()).To(Equal("independent"))

			bare := &pipeline.Stage{JobID: id, StageName: "classify", Status: pipeline.StageStatusCompleted}
			Expect(svc.PanelProvenance([]*pipeline.Stage{bare})["classify"].GetIndependence()).To(Equal("panel independence not recorded"))
			pending := &pipeline.Stage{JobID: id, StageName: "classify", Status: pipeline.StageStatusPending}
			Expect(svc.PanelProvenance([]*pipeline.Stage{pending})).NotTo(HaveKey("classify"))
			Expect(svc.PanelProvenance([]*pipeline.Stage{{JobID: id, StageName: "synthesis", Status: pipeline.StageStatusCompleted}})).To(BeEmpty())
		})

		It("shows the stored record and does not re-run the check when a job resumes after the stage completed", func() {
			id := start(false, pipeline.WithPanelProbe(llm))
			Expect(finished(id).Status).To(Equal(pipeline.JobStatusCompleted))
			probes := len(llm.CompleteRequests())
			Expect(probes).To(Equal(2))

			// Interrupt the job after classify: it resumes at synthesis. The
			// gateway now answers with a shared model, which a second check
			// would refuse.
			Expect(jobs.ResetStagesFrom(ctx, id, "synthesis", []string{"classify", "synthesis", "graph"})).To(Succeed())
			jobs.mu.Lock()
			jobs.jobs[id].Status = pipeline.JobStatusRunning
			jobs.mu.Unlock()
			models["panel-1"], models["panel-2"] = "a", "a"
			Expect(svc.Start(ctx)).To(Succeed())
			Eventually(func() pipeline.JobStatus {
				j, _ := jobs.GetJob(ctx, id)
				return j.Status
			}).Should(Equal(pipeline.JobStatusCompleted))

			Expect(llm.CompleteRequests()).To(HaveLen(probes), "the check is not run again")
			got := svc.PanelProvenance([]*pipeline.Stage{stageOf(id, "classify")})["classify"]
			Expect(got.GetIndependence()).To(Equal("independent"))
			Expect(got.GetModels()).To(HaveLen(2))
			Expect(got.GetModels()[0].GetObservedModel()).To(Equal("a"))
			Expect(got.GetModels()[1].GetObservedModel()).To(Equal("b"))
			Expect(got.GetModels()[0].GetSource()).To(Equal("reply"))
		})

		It("runs the job when the allowance is set", func() {
			models["panel-2"] = "a"
			models["panel-1"] = "a"
			Expect(finished(start(true, pipeline.WithPanelProbe(llm))).Status).To(Equal(pipeline.JobStatusCompleted))
		})

		It("runs the job without a panel probe, as before", func() {
			Expect(finished(start(false)).Status).To(Equal(pipeline.JobStatusCompleted))
			Expect(llm.CompleteRequests()).To(BeEmpty())
		})
	})
})
