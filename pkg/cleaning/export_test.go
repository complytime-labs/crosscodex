package cleaning

// DecodeKeys exposes decodeKeys, the strict decoder of a stored concerns or
// targets column, to the fuzz targets in package cleaning_test, at the
// concerns column's bound.
func DecodeKeys(b []byte) ([]VerdictKey, error) { return decodeKeys(b, maxEvidenceBytes) }

// SetStoredListLimit lowers the targets and edge_ids write bound to n bytes,
// so a spec can exercise it without a 128 MiB value, and returns a func that
// restores it (pass it to DeferCleanup).
func SetStoredListLimit(n int) func() {
	old := storedListLimit
	storedListLimit = n
	return func() { storedListLimit = old }
}
