package results

import (
	"fmt"
	"strings"
)

// CritiqueUpheldProperty, CritiqueRejectedProperty and CritiqueNoteProperty
// are the properties a kept SEMANTIC_MATCH or REQUIRES edge carries when its
// job ran the critique stage: the critique's UPHOLD and REJECT vote counts and,
// when non-empty, CritiqueResult.Note. Edges of jobs without a critique carry
// none of them.
const (
	CritiqueUpheldProperty   = "critique_upheld"
	CritiqueRejectedProperty = "critique_rejected"
	CritiqueNoteProperty     = "critique_note"
)

// GraphDeferredMetadataKey is the stage-completed event metadata key the
// pipeline sets to "true" on the relationship and requires events of a job
// whose stage rows include critique. The graph subscriber then writes no
// edges for the event: the critique event writes the surviving ones, because
// a graph edge cannot be deleted once a veto arrives.
const GraphDeferredMetadataKey = "graph_deferred"

// critiqueRejectVerdict is the verdict word of a REJECT entry in
// CritiqueResult.Reasons.
const critiqueRejectVerdict = "REJECT: "

// Note is the review note of a kept verdict: "critique split: <r> of <n>
// rejected — <first reject reason>" for a split critique, where n is the panel
// size, and "critique failed: <f> of <n> votes invalid" for a failed one. The
// reason is the first REJECT entry of Reasons as "<model> s<N>: <reason>"; a
// split critique without one names only the counts. Upheld and vetoed
// critiques have no note: an upheld verdict needs no review and a vetoed one
// is dropped.
func (c CritiqueResult) Note() string {
	panel := c.Upheld + c.Rejected + c.Failed
	switch c.Outcome {
	case CritiqueSplit:
		note := fmt.Sprintf("critique split: %d of %d rejected", c.Rejected, panel)
		if reason, ok := c.firstRejectReason(); ok {
			note += " — " + reason
		}
		return note
	case CritiqueFailed:
		return fmt.Sprintf("critique failed: %d of %d votes invalid", c.Failed, panel)
	default:
		return ""
	}
}

// firstRejectReason returns the first Reasons entry whose verdict is REJECT,
// relabelled "<model> s<N>: <reason>". An entry is "<model> s<N> <VERDICT>:
// <reason>", so the verdict is the third space-separated field; a REJECT later
// in the reason text does not count.
func (c CritiqueResult) firstRejectReason() (string, bool) {
	for _, r := range c.Reasons {
		fields := strings.SplitN(r, " ", 3)
		if len(fields) == 3 && strings.HasPrefix(fields[2], critiqueRejectVerdict) {
			return fields[0] + " " + fields[1] + ": " + strings.TrimPrefix(fields[2], critiqueRejectVerdict), true
		}
	}
	return "", false
}

// critiquePair keys a critique by its source and target controls.
type critiquePair struct{ source, target string }

// Verdicts is the critique stage's decision on one kind of verdict
// (relationship or requires) of one job, by pair. A pair the critique has no
// result for is kept with no note: the critique produced nothing that could
// justify dropping it. The zero value vetoes nothing and notes nothing, as
// does the value Apply returns for a job that ran no critique.
type Verdicts struct {
	byPair map[critiquePair]CritiqueResult
}

// Apply returns the critique decisions of the kind of verdict named kind
// (RelationshipAnalyzer or RequiresAnalyzer) from a job's critique results.
// Results of the other kind are ignored, so a pair vetoed as a relationship
// keeps its REQUIRES verdict and the reverse. The critique stores one result
// per (kind, pair); were a pair listed twice, the first is used. Apply is
// pure: applying it again to the same results gives the same decisions.
func Apply(kind string, crit []CritiqueResult) Verdicts {
	v := Verdicts{byPair: make(map[critiquePair]CritiqueResult, len(crit))}
	for _, c := range crit {
		if c.Kind != kind {
			continue
		}
		key := critiquePair{c.SourceControlID, c.TargetControlID}
		if _, dup := v.byPair[key]; !dup {
			v.byPair[key] = c
		}
	}
	return v
}

// Result returns the critique of the pair source → target, and false when the
// critique has none.
func (v Verdicts) Result(source, target string) (CritiqueResult, bool) {
	c, ok := v.byPair[critiquePair{source, target}]
	return c, ok
}

// Vetoed reports whether the critique vetoed the pair source → target: the
// verdict is dropped from the graph, synthesis and the default exports.
func (v Verdicts) Vetoed(source, target string) bool {
	c, ok := v.Result(source, target)
	return ok && c.Vetoed
}

// Note returns the review note of the pair source → target (see
// CritiqueResult.Note), or "" when there is none.
func (v Verdicts) Note(source, target string) string {
	c, ok := v.Result(source, target)
	if !ok {
		return ""
	}
	return c.Note()
}
