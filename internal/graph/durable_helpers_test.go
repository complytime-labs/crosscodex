package graph_test

import (
	"context"
	"encoding/json"

	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Durable consumer names the graph role owns on the MATERIALIZE stream.
const (
	materializerConsumer = "graph-materializer"
	barrierConsumer      = "graph-barrier"
)

// tenantCtx returns a context carrying tenantID, as a durable consumer hands
// its handler the tenant from the message's provenance.
func tenantCtx(tenantID string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

// stageEvent is the stage.completed message analyzer publishes for job-1 of
// tenantID, on its delivery-th delivery, with tenantID as its provenance
// tenant.
func stageEvent(tenantID, analyzer string, delivery uint64) *natsbus.Message {
	data, err := json.Marshal(map[string]string{"analyzer": analyzer, "job_id": "job-1", "stage": "completed"})
	Expect(err).NotTo(HaveOccurred())
	return &natsbus.Message{
		Subject:  "crosscodex.pipeline." + tenantID + ".job-1.stage.completed",
		Data:     data,
		Metadata: natsbus.MessageMetadata{TenantID: tenantID, NumDelivered: delivery},
	}
}

// consumerSub returns the durable consumer named durable that the Fake
// recorded last, or nil.
func consumerSub(bus *natsbustest.Fake, durable string) *natsbustest.Sub {
	subs := bus.Subscriptions()
	for i := len(subs) - 1; i >= 0; i-- {
		if subs[i].Config.Durable == durable {
			return subs[i]
		}
	}
	return nil
}

// eventCount reads the graph.events.total point for analyzer and status, 0
// when it was never recorded.
func eventCount(tp *telemetrytest.TestProvider, analyzer, status string) int64 {
	m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.events.total")
	if m == nil {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	Expect(ok).To(BeTrue())
	for _, dp := range sum.DataPoints {
		a, _ := dp.Attributes.Value("analyzer")
		st, _ := dp.Attributes.Value("status")
		if a.AsString() == analyzer && st.AsString() == status {
			return dp.Value
		}
	}
	return 0
}
