//go:build !integration

package main

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// fixedEmbeddingLLMClient is a hermetic llmclient.Client that returns one
// fixed vector per input, so the worker role can process an embedding task
// without an LLM gateway.
type fixedEmbeddingLLMClient struct{}

var _ llmclient.Client = fixedEmbeddingLLMClient{}

func (fixedEmbeddingLLMClient) Complete(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, llmclient.ErrNoGateway
}

func (fixedEmbeddingLLMClient) Embed(_ context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	data := make([]llmclient.EmbeddingData, len(req.Input))
	for i := range req.Input {
		data[i] = llmclient.EmbeddingData{Index: i, Embedding: []float32{0.1, 0.2, 0.3}}
	}
	return &llmclient.EmbeddingResponse{Data: data, Model: req.Model}, nil
}

func (fixedEmbeddingLLMClient) Health(context.Context) error { return nil }
func (fixedEmbeddingLLMClient) Close() error                 { return nil }

// The worker role needs only embedded NATS and an LLM client, so it is the
// one role whose bootstrap wiring can be exercised hermetically: this spec
// proves bootstrap hands the global otel providers to the services it builds
// rather than leaving them on their no-op defaults.
var _ = Describe("bootstrap telemetry wiring", func() {
	var tp *telemetrytest.TestProvider

	BeforeEach(func() {
		DeferCleanup(testspecs.IsolateTelemetryGlobals())
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tp.Shutdown, context.Background())
		otel.SetTracerProvider(tp.TracerProvider())
		otel.SetMeterProvider(tp.MeterProvider())
	})

	It("instruments the worker and its NATS client with the global providers", func() {
		const (
			tenantID = "telemetry-wiring"
			jobID    = "job-telemetry-wiring"
			taskID   = "task-telemetry-wiring"
		)
		ctx := testspecs.SetupTenantContext(tenantID)

		cfg := &config.Config{Role: config.RoleWorker}
		cfg.NATS.URL = ""
		cfg.NATS.Embedded.StoreDir = GinkgoT().TempDir()
		cfg.NATS.Streams.AuditLLMRetention = 90 * 24 * time.Hour
		cfg.NATS.Streams.AuditEventsRetention = 30 * 24 * time.Hour
		// llmclient.NewClient runs before WithLLMClient replaces its result,
		// so it needs a gateway URL; the fake client means it is never dialed.
		cfg.LLM.GatewayURL = "http://127.0.0.1:1"
		cfg.Health.Addr = "127.0.0.1:0"

		rt, err := bootstrap(ctx, cfg, WithLLMClient(fixedEmbeddingLLMClient{}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(rt.close)
		Expect(rt.start(ctx)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(rt.stop(ctx)).To(Succeed()) })

		bus := rt.shared.natsClient

		// Subscribe before publishing: core NATS does not replay, so a
		// result published before the subscription exists is lost.
		resultSubject, err := natsbus.ResultSubject(tenantID, natsbus.TaskEmbed, jobID)
		Expect(err).NotTo(HaveOccurred())
		results := make(chan *natsbus.Message, 1)
		sub, err := bus.Subscribe(ctx, resultSubject, func(_ context.Context, msg *natsbus.Message) error {
			if ids := msg.Headers["X-Task-Id"]; len(ids) > 0 && ids[0] == taskID {
				select {
				case results <- msg:
				default:
				}
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(sub.Unsubscribe)

		payload, err := structpb.NewStruct(map[string]any{
			"text":       "Access control policy",
			"model":      "test-embed",
			"batch_size": float64(1),
		})
		Expect(err).NotTo(HaveOccurred())
		data, err := proto.Marshal(payload)
		Expect(err).NotTo(HaveOccurred())
		workSubject, err := natsbus.WorkSubject(tenantID, natsbus.TaskEmbed, jobID)
		Expect(err).NotTo(HaveOccurred())
		Expect(bus.PublishWithHeaders(ctx, workSubject, data, map[string][]string{
			"X-Task-Id":     {taskID},
			"X-Task-Type":   {string(natsbus.TaskEmbed)},
			"X-Job-Id":      {jobID},
			"X-Retry-Count": {"0"},
		})).To(Succeed())

		var result *natsbus.Message
		Eventually(results, 10*time.Second).Should(Receive(&result))
		Expect(result.Headers).NotTo(HaveKey("X-Error"), "worker reported a task error")

		scopes := func() map[string]bool {
			names := map[string]bool{}
			for _, s := range tp.GetSpans() {
				names[s.InstrumentationScope().Name] = true
			}
			return names
		}
		Eventually(scopes).Should(SatisfyAll(
			HaveKey("crosscodex/internal/worker"),
			HaveKey("crosscodex/pkg/natsbus"),
		))
	})
})
