package feedback

// Bridges for the fake Store in package feedback_test, so that it refuses
// what PGStore refuses with the same checks and messages.
var (
	CheckDetermine  = checkDetermine
	CheckStale      = checkStale
	ReviewChanged   = reviewChanged
	Unprojected     = unprojected
	MappingNotFound = mappingNotFound
	CheckEdgeIDs    = checkEdgeIDs
)

// Count32 is the narrowing guard for int32 proto counts, for its boundary table.
var Count32 = count32

// Aggregate is the service's vote aggregation, for the aggregation table
// and property specs.
var Aggregate = aggregate

// SetStoredEdgeIDsLimit lowers the edge_ids write bound to n bytes, so a
// spec can exercise it without a 128 MiB value, and returns a func that
// restores it (pass it to DeferCleanup).
func SetStoredEdgeIDsLimit(n int) func() {
	old := storedEdgeIDsLimit
	storedEdgeIDsLimit = n
	return func() { storedEdgeIDsLimit = old }
}

// DecodePageToken and EncodePageToken bridge the review queue's page token
// codec for its fuzz target.
func DecodePageToken(s string) (source, target string, err error) {
	t, err := decodePageToken(s)
	return t.Source, t.Target, err
}

func EncodePageToken(source, target string) string {
	return pageToken{Source: source, Target: target}.encode()
}
