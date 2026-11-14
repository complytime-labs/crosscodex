package crosswalk

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// Path is one composed a→hub→c path.
type Path struct {
	Hub        string              // hub control identifier
	Type       pb.RelationshipType // composed type; UNSPECIFIED when Compose named none
	Confidence float64             // product of the two legs
	Jobs       []string            // IDs of every job on either leg
	LatestJob  string              // the newer leg's deciding job
	LatestAt   time.Time           // that job's completion time
}

// Collapsed is one derived row for an (a, c) pair.
type Collapsed struct {
	Type            pb.RelationshipType // UNSPECIFIED reads "related (derived, type unknown)"
	Confidence      float64
	Hubs            []string // distinct, sorted
	Jobs            []string // distinct, sorted, every leg of every path
	JobsAgreeing    int      // distinct jobs on supporting paths
	LatestJob       string   // newest supporting LatestJob (LatestAt, then greater ID)
	PathsSupporting int
	PathsTotal      int
	Notes           []string // review notes, disagreement then minority
}

// Collapse reduces every via path of one (a, c) pair to a single verdict.
// paths must be non-empty; empty input returns the zero Collapsed.
//
// Verdict: paths with Type UNSPECIFIED name nothing. If all named paths share
// one type, that type is the verdict and only those paths support it.
// Disagreement: if no path is named, or named paths name different types, the
// verdict is UNSPECIFIED ("type unknown"), every path supports it, and a
// disagreement adds a note listing each named path.
// Minority: a named verdict supported by fewer than half the paths adds a note.
//
// Confidence is the maximum over supporting paths. Verdicts are never
// rewritten: notes only report.
func Collapse(paths []Path) Collapsed {
	if len(paths) == 0 {
		return Collapsed{}
	}
	var named []Path
	for _, p := range paths {
		if p.Type != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
			named = append(named, p)
		}
	}
	verdict := pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
	if len(named) > 0 && !slices.ContainsFunc(named, func(p Path) bool { return p.Type != named[0].Type }) {
		verdict = named[0].Type
	}

	supporting := paths
	if verdict != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
		supporting = named
	}

	c := Collapsed{
		Type:            verdict,
		PathsSupporting: len(supporting),
		PathsTotal:      len(paths),
	}
	var latestAt time.Time
	agreeing := map[string]struct{}{}
	for i, p := range supporting {
		c.Confidence = max(c.Confidence, p.Confidence)
		if i == 0 || p.LatestAt.After(latestAt) || (p.LatestAt.Equal(latestAt) && p.LatestJob > c.LatestJob) {
			latestAt, c.LatestJob = p.LatestAt, p.LatestJob
		}
		for _, j := range p.Jobs {
			agreeing[j] = struct{}{}
		}
	}
	c.JobsAgreeing = len(agreeing)

	hubs, jobs := map[string]struct{}{}, map[string]struct{}{}
	for _, p := range paths {
		hubs[p.Hub] = struct{}{}
		for _, j := range p.Jobs {
			jobs[j] = struct{}{}
		}
	}
	c.Hubs = sortedKeys(hubs)
	c.Jobs = sortedKeys(jobs)

	if verdict == pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED && len(named) > 0 {
		c.Notes = append(c.Notes, disagreementNote(named))
	}
	if verdict != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED && 2*c.PathsSupporting < c.PathsTotal {
		c.Notes = append(c.Notes, fmt.Sprintf("'%s' rests on %d of %d paths", Word(verdict), c.PathsSupporting, c.PathsTotal))
	}
	return c
}

// disagreementNote lists "<word> via <hub>" for each named path, ordered by
// hub then word so the note does not depend on input order.
func disagreementNote(named []Path) string {
	sorted := slices.Clone(named)
	slices.SortFunc(sorted, func(a, b Path) int {
		return cmp.Or(strings.Compare(a.Hub, b.Hub), strings.Compare(Word(a.Type), Word(b.Type)))
	})
	entries := make([]string, len(sorted))
	for i, p := range sorted {
		entries[i] = Word(p.Type) + " via " + p.Hub
	}
	return "paths disagree: " + strings.Join(entries, ", ")
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
