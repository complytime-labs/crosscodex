package results

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// wireContribution is one element of relationship_candidates.provenance as
// the pipeline writes it: a marshalled requires.CandidateProvenance, keyed by
// its Go field names. internal packages do not import each other, so the
// shape is mirrored here and decodeContributions rejects any drift.
type wireContribution struct {
	GeneratorName string
	Score         float64
	Weight        float64
	Metadata      map[string]string
}

// decodeContributions parses a provenance document. The column is data a
// pipeline run wrote, but it is decoded strictly: an unexpected shape means
// the row was corrupted or written by an incompatible version, and a caller
// computing quality metrics from it must not guess.
func decodeContributions(raw []byte) ([]Contribution, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire []wireContribution
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptProvenance, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the provenance array", ErrCorruptProvenance)
	}
	if wire == nil {
		return nil, nil
	}
	out := make([]Contribution, len(wire))
	for i, w := range wire {
		if w.GeneratorName == "" {
			return nil, fmt.Errorf("%w: contribution %d has no GeneratorName", ErrCorruptProvenance, i)
		}
		if !isFinite(w.Score) || !isFinite(w.Weight) {
			return nil, fmt.Errorf("%w: contribution %d (%s) has a non-finite score or weight", ErrCorruptProvenance, i, w.GeneratorName)
		}
		out[i] = Contribution{Generator: w.GeneratorName, Score: w.Score, Weight: w.Weight, Metadata: w.Metadata}
	}
	return out, nil
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
