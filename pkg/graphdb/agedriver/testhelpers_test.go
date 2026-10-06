package agedriver_test

import (
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// queryMetrics returns what the driver recorded for operation op: the
// graphdb.queries.total count keyed by "<status>/<result>" (for example
// "ok/exists"), and the number of graphdb.query.duration_ms observations. An
// instrument that recorded nothing yet contributes nothing.
func queryMetrics(tp *telemetrytest.TestProvider, op string) (map[string]int64, uint64) {
	rm := tp.GetMetrics()
	byOutcome := map[string]int64{}
	if m := telemetrytest.FindMetric(rm, "graphdb.queries.total"); m != nil {
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue(), "graphdb.queries.total is %T, not Sum[int64]", m.Data)
		for _, dp := range sum.DataPoints {
			if v, _ := dp.Attributes.Value("operation"); v.AsString() == op {
				status, _ := dp.Attributes.Value("status")
				result, _ := dp.Attributes.Value("result")
				byOutcome[status.AsString()+"/"+result.AsString()] += dp.Value
			}
		}
	}
	var latencies uint64
	if m := telemetrytest.FindMetric(rm, "graphdb.query.duration_ms"); m != nil {
		hist, ok := m.Data.(metricdata.Histogram[int64])
		Expect(ok).To(BeTrue(), "graphdb.query.duration_ms is %T, not Histogram[int64]", m.Data)
		for _, dp := range hist.DataPoints {
			if v, _ := dp.Attributes.Value("operation"); v.AsString() == op {
				latencies += dp.Count
			}
		}
	}
	return byOutcome, latencies
}
