package analysis

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// ExportStageErrorCategory exposes stageErrorCategory for specs.
var ExportStageErrorCategory = stageErrorCategory

// ExportComputeBackoff exposes computeBackoff for property testing.
var ExportComputeBackoff = computeBackoff

// Header constants for test assertions.
const (
	ExportHeaderTaskID     = headerTaskID
	ExportHeaderTaskType   = headerTaskType
	ExportHeaderJobID      = headerJobID
	ExportHeaderRetryCount = headerRetryCount
	// ExportHeaderCollectionID is the work header naming a task's collection.
	ExportHeaderCollectionID = headerCollectionID
)

// ExportRelease exposes CollectionHandle.release.
func (h *CollectionHandle) ExportRelease() error { return h.release() }

// ExportStageProgress exposes stageProgress for specs.
type ExportStageProgress = stageProgress

// ExportNewStageProgress builds a stageProgress whose metrics come from mp.
func ExportNewStageProgress(store ProgressStore, jobID, stage string, interval time.Duration,
	logger *slog.Logger, mp metric.MeterProvider) *stageProgress {
	return newStageProgress(store, jobID, stage, interval, logger, nil, newProgressMetrics(mp))
}

// ExportFlush exposes stageProgress.flush, standing in for a tick.
func (p *stageProgress) ExportFlush(ctx context.Context) error { return p.flush(ctx) }

// ExportOrderByModel exposes orderByModel for property testing.
var ExportOrderByModel = orderByModel
