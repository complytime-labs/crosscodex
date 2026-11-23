package cleaning

// DecodeKeys exposes decodeKeys, the strict decoder of a stored concerns or
// targets column, to the fuzz targets in package cleaning_test, at the
// concerns column's bound.
func DecodeKeys(b []byte) ([]VerdictKey, error) { return decodeKeys(b, maxEvidenceBytes) }

// Governors exposes governors to package cleaning_test. ds[i] is fs[i]'s
// latest retire or restore decision; the result names, per target verdict,
// the finding whose decision governs it.
func Governors(fs []StoredFinding, ds []Decision) map[VerdictKey]string {
	cs := make([]reprojectCandidate, len(fs))
	for i := range fs {
		cs[i] = reprojectCandidate{f: fs[i], d: ds[i]}
	}
	out := map[VerdictKey]string{}
	for k, i := range governors(cs) {
		out[k] = fs[i].ID
	}
	return out
}

// SetStoredListLimit lowers the targets and edge_ids write bound to n bytes,
// so a spec can exercise it without a 128 MiB value, and returns a func that
// restores it (pass it to DeferCleanup).
func SetStoredListLimit(n int) func() {
	old := storedListLimit
	storedListLimit = n
	return func() { storedListLimit = old }
}
