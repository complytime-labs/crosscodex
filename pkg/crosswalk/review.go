package crosswalk

import (
	"fmt"
	"slices"
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// EquivalenceNotes flags one-to-many EQUIVALENT verdicts for human review.
// The result is aligned with rows: "" where a row has no note. Only direct
// EQUIVALENT rows take part. Rows are grouped by node ID, never by published
// identifier, so two nodes sharing an identifier both appear.
//
//   - A source equivalent to 2 or more targets in one target catalog: each of
//     its rows notes "one of <n> 'equivalent' targets for <source>: <targets>".
//   - A target equivalent to 2 or more sources in one source catalog: each of
//     its rows notes "one of <n> 'equivalent' sources for <target>: <sources>".
//
// A row in both kinds of group gets both notes, targets first, joined by "; ".
// Verdicts are never rewritten: notes only report. Callers pass the full
// filtered row set so notes do not depend on the page.
func EquivalenceNotes(rows []*pb.CrosswalkRow) []string {
	var edges []EquivalenceEdge
	var rowOf []int // edges index -> rows index
	for i, r := range rows {
		if r.RelationshipType != pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT || r.Direction != string(Direct) {
			continue
		}
		edges = append(edges, EquivalenceEdge{r.Source.NodeId, r.Source.CatalogId, r.Target.NodeId, r.Target.CatalogId})
		rowOf = append(rowOf, i)
	}

	targetNotes := make([]string, len(rows))
	sourceNotes := make([]string, len(rows))
	for _, g := range EquivalenceGroups(edges) {
		word, out := "sources", sourceNotes
		subject := func(r *pb.CrosswalkRow) string { return r.Target.Identifier }
		listed := func(r *pb.CrosswalkRow) string { return r.Source.Identifier }
		if g.NodeIsSource {
			word, out = "targets", targetNotes
			subject, listed = listed, subject
		}
		ids := make([]string, len(g.Members))
		for j, m := range g.Members {
			ids[j] = listed(rows[rowOf[m]])
		}
		slices.Sort(ids)
		note := fmt.Sprintf("one of %d 'equivalent' %s for %s: %s", len(ids), word, subject(rows[rowOf[g.Members[0]]]), strings.Join(ids, ", "))
		for _, m := range g.Members {
			out[rowOf[m]] = note
		}
	}

	notes := make([]string, len(rows))
	for i := range rows {
		notes[i] = strings.Join(slices.DeleteFunc([]string{targetNotes[i], sourceNotes[i]}, func(s string) bool { return s == "" }), "; ")
	}
	return notes
}
