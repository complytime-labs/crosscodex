package analysis_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("stage progress", func() {
		var (
			tp  *telemetrytest.TestProvider
			ctx context.Context
		)

		BeforeAll(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			ctx, err = tenant.WithTenant(context.Background(), "progress-tenant")
			Expect(err).NotTo(HaveOccurred())
		})

		It("writes every event exactly once, so stored done always equals succeeded plus failed", func() {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				store := &recordingProgressStore{}
				// An hour-long interval keeps the ticker out of the run; the
				// drawn flushes stand in for its ticks.
				p := analysis.ExportNewStageProgress(store, "job-p", "classify", time.Hour, logger, tp.MeterProvider())
				total := rapid.IntRange(0, 500).Draw(t, "total")
				cacheHits := rapid.IntRange(0, 50).Draw(t, "cacheHits")
				preBuilt := cacheHits + rapid.IntRange(0, 50).Draw(t, "otherPreBuilt")
				p.Start(ctx, total, preBuilt, cacheHits)

				var succeeded, failed, retried int
				ops := rapid.SliceOfN(rapid.IntRange(0, 4), 0, 200).Draw(t, "ops")
				for _, op := range ops {
					switch op {
					case 0:
						p.TaskSucceeded()
						succeeded++
					case 1:
						p.TaskFailed()
						failed++
					case 2:
						p.TaskRetried()
						retried++
					case 3:
						store.failWith(nil)
						if err := p.ExportFlush(ctx); err != nil {
							t.Fatalf("flush to a working store failed: %v", err)
						}
					case 4:
						store.failWith(errors.New("database unavailable"))
						// Fails when events are pending; with none it writes nothing.
						_ = p.ExportFlush(ctx)
					}
				}
				store.failWith(nil)
				if err := p.Finish(ctx); err != nil {
					t.Fatalf("Finish: %v", err)
				}

				var sumSucceeded, sumFailed, sumRetried, sumCache int
				for _, c := range store.recorded() {
					d := c.delta
					if d.Total != total {
						t.Fatalf("delta total %d, want the stage's %d", d.Total, total)
					}
					if d.Succeeded < 0 || d.Failed < 0 || d.Retried < 0 || d.CacheHits < 0 {
						t.Fatalf("negative delta %+v", d)
					}
					sumSucceeded += d.Succeeded
					sumFailed += d.Failed
					sumRetried += d.Retried
					sumCache += d.CacheHits
				}
				if sumSucceeded != succeeded+preBuilt || sumFailed != failed || sumRetried != retried || sumCache != cacheHits {
					t.Fatalf("stored succeeded=%d failed=%d retried=%d cache_hits=%d, want %d %d %d %d",
						sumSucceeded, sumFailed, sumRetried, sumCache, succeeded+preBuilt, failed, retried, cacheHits)
				}
				if done := sumSucceeded + sumFailed; done != succeeded+preBuilt+failed {
					t.Fatalf("stored done %d, want %d tasks finished", done, succeeded+preBuilt+failed)
				}
			})
		})
	})
})
