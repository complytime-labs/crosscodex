package crosswalk

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// shortJobLen is how many characters of a job ID FormatOthers prints.
const shortJobLen = 8

// Verdict is one job's judgement of a control pair.
type Verdict struct {
	JobID       string
	CompletedAt time.Time
	Type        pb.RelationshipType
	Confidence  float64
	// Reversed is true when the job judged the pair target-to-source.
	Reversed bool
}

// Merged is the consensus of every job's verdict on one pair, in the
// source-to-target direction.
type Merged struct {
	Type       pb.RelationshipType
	Direction  Direction
	Confidence float64
	// LatestJob is the job whose verdict supplied Type and Confidence.
	LatestJob string
	// JobsJudged counts distinct jobs; JobsAgreeing counts distinct jobs with
	// a normalized verdict equal to the latest one.
	JobsJudged, JobsAgreeing int
	// Others holds the non-latest normalized verdicts, newest first.
	Others []Verdict
}

// normalize puts v in the source-to-target direction. A reversed verdict whose
// type cannot be inverted stays as-is with Reversed set, so it never equals a
// forward verdict.
func normalize(v Verdict) Verdict {
	if !v.Reversed {
		return v
	}
	if inv, ok := Invert(v.Type); ok {
		v.Type, v.Reversed = inv, false
	}
	return v
}

// newerFirst orders verdicts newest first. Ties on time and job fall through
// to the remaining fields so the order, and therefore Merge, is total and
// independent of input order.
func newerFirst(a, b Verdict) int {
	return cmp.Or(
		b.CompletedAt.Compare(a.CompletedAt),
		strings.Compare(b.JobID, a.JobID),
		cmp.Compare(b.Confidence, a.Confidence),
		cmp.Compare(b.Type, a.Type),
		compareBool(a.Reversed, b.Reversed),
	)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

// Merge combines verdicts from several jobs on one pair. The latest verdict
// (CompletedAt, ties by greater JobID) supplies Type, Confidence and
// LatestJob. Empty input returns the zero Merged.
//
// One job may supply several verdicts on a pair: a bidirectional job judges it
// in both directions. The job counts once in JobsJudged and JobsAgreeing, and
// every one of its verdicts is kept, so Others shows a direction that
// disagrees. Among a job's own verdicts the first in newerFirst order decides
// the row: the higher confidence, then the greater relationship type, then the
// forward reading.
func Merge(vs []Verdict) Merged {
	if len(vs) == 0 {
		return Merged{}
	}
	norm := make([]Verdict, len(vs))
	for i, v := range vs {
		norm[i] = normalize(v)
	}
	slices.SortFunc(norm, newerFirst)

	latest := norm[0]
	m := Merged{
		Type:       latest.Type,
		Direction:  Direct,
		Confidence: latest.Confidence,
		LatestJob:  latest.JobID,
		Others:     norm[1:],
	}
	if latest.Reversed {
		m.Direction = Reverse
	}
	judged := map[string]struct{}{}
	agreeing := map[string]struct{}{}
	for _, v := range norm {
		judged[v.JobID] = struct{}{}
		if v.Type == latest.Type && v.Reversed == latest.Reversed {
			agreeing[v.JobID] = struct{}{}
		}
	}
	m.JobsJudged, m.JobsAgreeing = len(judged), len(agreeing)
	return m
}

// FormatOthers renders verdicts as "<word>@<confidence> (<short job>)" joined
// by "; ". A reversed verdict (not invertible) is prefixed "reverse: ".
func FormatOthers(vs []Verdict) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		word := Word(v.Type)
		if v.Reversed {
			word = "reverse: " + word
		}
		job := v.JobID
		if len(job) > shortJobLen {
			job = job[:shortJobLen]
		}
		parts[i] = fmt.Sprintf("%s@%.2f (%s)", word, v.Confidence, job)
	}
	return strings.Join(parts, "; ")
}
