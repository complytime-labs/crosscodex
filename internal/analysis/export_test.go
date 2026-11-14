package analysis

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

// ExportOrderByModel exposes orderByModel for property testing.
var ExportOrderByModel = orderByModel
