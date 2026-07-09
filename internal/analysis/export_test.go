package analysis

// ExportComputeBackoff exposes computeBackoff for property testing.
var ExportComputeBackoff = computeBackoff

// Header constants for test assertions.
const (
	ExportHeaderTaskID     = headerTaskID
	ExportHeaderTaskType   = headerTaskType
	ExportHeaderJobID      = headerJobID
	ExportHeaderRetryCount = headerRetryCount
)
