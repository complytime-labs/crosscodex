//go:build !integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("buildSharedResources LLM audit wiring", func() {
	It("refuses to open the LLM client without NATS, so no LLM call goes unaudited", func() {
		cfg := &config.Config{}
		cfg.LLM.GatewayURL = "http://127.0.0.1:1"

		res, err := buildSharedResources(context.Background(), cfg, requiredResources{llm: true})

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("LLM audit events are published to NATS"))
		Expect(err.Error()).To(ContainSubstring("request the nats resource"))
		Expect(res).To(BeNil())
	})

	It("publishes an LLM audit event to the tenant's audit subject for each completion", func() {
		const (
			tenantID = "llm-audit-wiring"
			jobID    = "job-llm-audit-wiring"
		)
		ctx := testspecs.SetupTenantContext(tenantID)

		gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","model":"m1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		}))
		DeferCleanup(gateway.Close)

		cfg := &config.Config{}
		cfg.NATS.Embedded.StoreDir = GinkgoT().TempDir()
		cfg.NATS.Streams.AuditLLMRetention = 90 * 24 * time.Hour
		cfg.NATS.Streams.AuditEventsRetention = 30 * 24 * time.Hour
		cfg.LLM.GatewayURL = gateway.URL

		res, err := buildSharedResources(ctx, cfg, requiredResources{nats: true, llm: true})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.close)

		subject, err := natsbus.AuditSubject(tenantID, natsbus.AuditLLM, jobID)
		Expect(err).NotTo(HaveOccurred())
		events := make(chan llmclient.AuditEvent, 1)
		sub, err := res.natsClient.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
			var ev llmclient.AuditEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				return err
			}
			events <- ev
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(sub.Unsubscribe)

		_, err = res.llmClient.Complete(ctx, &llmclient.CompletionRequest{
			Model: "m1", Messages: []llmclient.ChatMessage{{Role: "user", Content: "hi"}},
			TenantID: tenantID, JobID: jobID, PromptName: "wiring_prompt", PromptVersion: "1.0.0",
		})
		Expect(err).NotTo(HaveOccurred())

		var ev llmclient.AuditEvent
		Eventually(events, 5*time.Second).Should(Receive(&ev))
		Expect(ev.TenantID).To(Equal(tenantID))
		Expect(ev.JobID).To(Equal(jobID))
		Expect(ev.Operation).To(Equal(llmclient.OpComplete))
		Expect(ev.PromptName).To(Equal("wiring_prompt"))
		Expect(ev.PromptVersion).To(Equal("1.0.0"))
		Expect(ev.Success).To(BeTrue())
	})
})
