package llmclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

const catalogBody = `{"data":[
 {"model_name":"panel-1","litellm_params":{"model":"openai/qwen3.8-flash-next","api_base":"http://10.0.0.9:4000","api_key":"SECRET","extra_body":{"a":1}},"model_info":{"id":"d1","x":2}},
 {"model_name":"panel-2","litellm_params":{"model":"hosted_vllm/gpt-oss-120b"},"model_info":{"id":"d2"}}]}`

var _ = Describe("ParseModelCatalog", func() {
	It("reads alias, deployment ID and upstream model, and nothing else", func() {
		got, err := llmclient.ParseModelCatalog([]byte(catalogBody))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]llmclient.ModelDeployment{
			{Alias: "panel-1", ID: "d1", Model: "openai/qwen3.8-flash-next"},
			{Alias: "panel-2", ID: "d2", Model: "hosted_vllm/gpt-oss-120b"},
		}))
	})

	DescribeTable("skips an entry whose fields have the wrong type, keeping the rest",
		func(entry string) {
			got, err := llmclient.ParseModelCatalog([]byte(`{"data":[` + entry + `,{"model_name":"ok","litellm_params":{"model":"m"},"model_info":{"id":"i"}}]}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]llmclient.ModelDeployment{{Alias: "ok", ID: "i", Model: "m"}}))
		},
		Entry("a numeric model_name", `{"model_name":5}`),
		Entry("a string litellm_params", `{"litellm_params":"x"}`),
		Entry("a list model_info", `{"model_info":[]}`),
		Entry("a non-object entry", `7`),
	)

	DescribeTable("rejects a body that is not a catalog",
		func(body string) {
			_, err := llmclient.ParseModelCatalog([]byte(body))
			Expect(err).To(MatchError(llmclient.ErrInvalidModelCatalog))
		},
		Entry("an empty body", ``),
		Entry("not JSON", `<html>`),
		Entry("a list", `[]`),
		Entry("no data", `{}`),
		Entry("data that is not a list", `{"data":{"a":1}}`),
		Entry("data null", `{"data":null}`),
	)

	It("keeps no more than the entry bound", func() {
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := range 10050 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"model_name":"a"}`)
		}
		b.WriteString(`]}`)
		got, err := llmclient.ParseModelCatalog([]byte(b.String()))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(10000))
	})
})

var _ = Describe("client ModelCatalog", func() {
	var (
		status int
		body   string
		auth   string
		path   string
		method string
	)
	newClient := func(opts ...llmclient.Option) llmclient.ModelCatalog {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, path, method = r.Header.Get("Authorization"), r.URL.Path, r.Method
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		DeferCleanup(srv.Close)
		c, err := llmclient.NewClient(config.LLMConfig{GatewayURL: srv.URL + "/", Timeout: 5}, opts...)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = c.Close() })
		cat, ok := c.(llmclient.ModelCatalog)
		Expect(ok).To(BeTrue(), "the real client implements ModelCatalog")
		return cat
	}
	BeforeEach(func() { status, body, auth, path, method = http.StatusOK, catalogBody, "", "", "" })

	It("reads GET /v1/model/info", func() {
		got, err := newClient().ModelCatalog(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(2))
		Expect(method).To(Equal(http.MethodGet))
		Expect(path).To(Equal("/v1/model/info"))
	})

	DescribeTable("classifies a failed read",
		func(code int, want error) {
			status = code
			_, err := newClient().ModelCatalog(context.Background())
			Expect(err).To(MatchError(want))
		},
		Entry("404 is an unsupported catalog", http.StatusNotFound, llmclient.ErrModelCatalogUnsupported),
		Entry("405 is an unsupported catalog", http.StatusMethodNotAllowed, llmclient.ErrModelCatalogUnsupported),
		Entry("401 is an authentication failure", http.StatusUnauthorized, llmclient.ErrAuthentication),
		Entry("403 is an authentication failure", http.StatusForbidden, llmclient.ErrAuthentication),
		Entry("500 is an unavailable gateway", http.StatusInternalServerError, llmclient.ErrGatewayUnavailable),
	)

	It("rejects a 200 that is not a catalog", func() {
		body = `{"status":"ok"}`
		_, err := newClient().ModelCatalog(context.Background())
		Expect(err).To(MatchError(llmclient.ErrInvalidModelCatalog))
	})

	It("refuses a body over the size bound", func() {
		body = `{"data":[],"pad":"` + strings.Repeat("x", 5*1024*1024) + `"}`
		_, err := newClient().ModelCatalog(context.Background())
		Expect(err).To(MatchError(llmclient.ErrResponseTooLarge))
	})

	It("reports an unreachable gateway as unavailable", func() {
		c, err := llmclient.NewClient(config.LLMConfig{GatewayURL: "http://127.0.0.1:1", Timeout: 1})
		Expect(err).NotTo(HaveOccurred())
		_, err = c.(llmclient.ModelCatalog).ModelCatalog(context.Background())
		Expect(err).To(MatchError(llmclient.ErrGatewayUnavailable))
	})

	It("counts reads by success", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		cat := newClient(llmclient.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, err = cat.ModelCatalog(context.Background())
		Expect(err).NotTo(HaveOccurred())
		status = http.StatusNotFound
		_, err = cat.ModelCatalog(context.Background())
		Expect(err).To(HaveOccurred())
		m := telemetrytest.FindMetric(tp.GetMetrics(), "llmclient.model_catalog.total")
		Expect(m).NotTo(BeNil())
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue())
		var total int64
		for _, dp := range sum.DataPoints {
			total += dp.Value
		}
		Expect(total).To(Equal(int64(2)))
		Expect(sum.DataPoints).To(HaveLen(2))
	})

	It("authenticates like a completion", func() {
		t := GinkgoT()
		t.Setenv("CATALOG_TEST_KEY", "k-123")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(catalogBody))
		}))
		DeferCleanup(srv.Close)
		c, err := llmclient.NewClient(config.LLMConfig{GatewayURL: srv.URL, APIKeyRef: "env:CATALOG_TEST_KEY"})
		Expect(err).NotTo(HaveOccurred())
		_, err = c.(llmclient.ModelCatalog).ModelCatalog(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(auth).To(Equal("Bearer k-123"))
	})
})
