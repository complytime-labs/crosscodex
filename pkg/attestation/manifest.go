package attestation

import (
	"fmt"
	"sort"
)

// GenerateManifest creates a SHA-256 manifest of artifacts.
// Each line is formatted as "<sha256-digest>  <uri>\n" (GNU coreutils format).
// The output is deterministic: artifacts are sorted by URI.
func GenerateManifest(artifacts []Artifact) []byte {
	if len(artifacts) == 0 {
		return nil
	}

	// Sort a copy to avoid mutating the caller's slice.
	sorted := make([]Artifact, len(artifacts))
	copy(sorted, artifacts)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].URI < sorted[j].URI
	})

	var buf []byte
	for _, a := range sorted {
		line := fmt.Sprintf("%s  %s\n", a.Digest, a.URI)
		buf = append(buf, line...)
	}
	return buf
}
