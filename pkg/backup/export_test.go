package backup

// SetMaxManifestBytes lowers Decode's size cap for a spec and returns a
// func restoring it, so the cap can be tested without a 256 MiB input.
func SetMaxManifestBytes(n int64) func() {
	old := maxManifestBytes
	maxManifestBytes = n
	return func() { maxManifestBytes = old }
}

// Bridges for step-level specs.
var (
	SnapshotObjects  = snapshotObjects
	PreflightObjects = preflightObjects
	RestoreObjects   = restoreObjects
	VerifyObjects    = verifyObjects

	SnapshotStreams  = snapshotStreams
	PreflightStreams = preflightStreams
	RestoreStreams   = restoreStreams
	VerifyStreams    = verifyStreams
)
