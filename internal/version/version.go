// Package version exposes build metadata (version, commit, date) populated
// via ldflags at compile time.
package version

import "runtime"

// Set via -ldflags at build time.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// Info holds all version and build metadata.
type Info struct {
	Version   string
	GitCommit string
	BuildDate string
	GoVersion string
	OS        string
	Arch      string
}

// GetInfo returns version and build metadata, including runtime environment.
func GetInfo() Info {
	return Info{
		Version:   Version,
		GitCommit: GitCommit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
