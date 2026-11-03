package pipeline_test

import (
	"context"
	"database/sql"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("Catalog reader telemetry", func() {
	var tp *telemetrytest.TestProvider

	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
	})

	statusCount := func(name, status string) int64 {
		m := telemetrytest.FindMetric(tp.GetMetrics(), name)
		Expect(m).NotTo(BeNil(), "metric %s", name)
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue())
		var n int64
		for _, dp := range sum.DataPoints {
			if s, _ := dp.Attributes.Value("status"); s.AsString() == status {
				n += dp.Value
			}
		}
		return n
	}

	durationCount := func(name string) uint64 {
		m := telemetrytest.FindMetric(tp.GetMetrics(), name)
		Expect(m).NotTo(BeNil(), "metric %s", name)
		n, err := telemetrytest.Float64HistogramCount(m)
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	errRead := errors.New("connection reset")

	similarity := func(tx *dbtest.Tx) error {
		r := pipeline.NewPGEmbeddingsReader(&dbtest.Conn{Tx: tx},
			pipeline.WithPGEmbeddingsReaderTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, _, err := r.SimilarityMatrix(context.Background(), "tenant-1", []string{"cat-src", "cat-tgt"}, "model-1")
		return err
	}
	catalogName := func(tx *dbtest.Tx) error {
		r := pipeline.NewPGCatalogNameReader(&dbtest.Conn{Tx: tx},
			pipeline.WithPGCatalogNameReaderTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		_, err := r.CatalogSnapshot(context.Background(), "tenant-1", "cat-1")
		return err
	}

	DescribeTable("records a span with tenant and operation, an operations counter and a duration histogram",
		func(read func(*dbtest.Tx) error, tx *dbtest.Tx, spanName, operation, metric, status string, code codes.Code) {
			err := read(tx)
			if status == "ok" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}

			span := telemetrytest.FindSpan(tp.GetSpans(), spanName)
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(code))
			v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("tenant-1"))
			v, ok = telemetrytest.SpanAttribute(span, "operation")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal(operation))

			Expect(statusCount(metric+".total", status)).To(Equal(int64(1)))
			Expect(durationCount(metric + ".duration_ms")).To(Equal(uint64(1)))
		},
		Entry("similarity matrix read", similarity,
			&dbtest.Tx{QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil }},
			"pipeline.embeddings.similarity_matrix", "similarity_matrix", "pipeline.embeddings.similarity_matrix", "ok", codes.Ok),
		Entry("similarity matrix query failure", similarity,
			&dbtest.Tx{QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return nil, errRead }},
			"pipeline.embeddings.similarity_matrix", "similarity_matrix", "pipeline.embeddings.similarity_matrix", "error", codes.Error),
		Entry("catalog name read", catalogName,
			&dbtest.Tx{QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(nil, "CSF 2.0", "2.0") }},
			"pipeline.catalogs.name", "catalog_name", "pipeline.catalogs.name", "ok", codes.Ok),
		Entry("catalog name not found", catalogName,
			&dbtest.Tx{QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(sql.ErrNoRows) }},
			"pipeline.catalogs.name", "catalog_name", "pipeline.catalogs.name", "error", codes.Error),
	)
})
