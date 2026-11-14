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
	type key struct{ node, otherCatalog string }
	targetGroups := map[key][]int{} // (source node, target catalog) -> row indexes
	sourceGroups := map[key][]int{} // (target node, source catalog) -> row indexes
	for i, r := range rows {
		if r.RelationshipType != pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT || r.Direction != string(Direct) {
			continue
		}
		tk := key{r.Source.NodeId, r.Target.CatalogId}
		targetGroups[tk] = append(targetGroups[tk], i)
		sk := key{r.Target.NodeId, r.Source.CatalogId}
		sourceGroups[sk] = append(sourceGroups[sk], i)
	}

	targetNotes := make([]string, len(rows))
	sourceNotes := make([]string, len(rows))
	describe := func(groups map[key][]int, word string, subject, listed func(*pb.CrosswalkRow) string, out []string) {
		for _, idx := range groups {
			if len(idx) < 2 {
				continue
			}
			ids := make([]string, len(idx))
			for j, i := range idx {
				ids[j] = listed(rows[i])
			}
			slices.Sort(ids)
			note := fmt.Sprintf("one of %d 'equivalent' %s for %s: %s", len(idx), word, subject(rows[idx[0]]), strings.Join(ids, ", "))
			for _, i := range idx {
				out[i] = note
			}
		}
	}
	describe(targetGroups, "targets", func(r *pb.CrosswalkRow) string { return r.Source.Identifier }, func(r *pb.CrosswalkRow) string { return r.Target.Identifier }, targetNotes)
	describe(sourceGroups, "sources", func(r *pb.CrosswalkRow) string { return r.Target.Identifier }, func(r *pb.CrosswalkRow) string { return r.Source.Identifier }, sourceNotes)

	notes := make([]string, len(rows))
	for i := range rows {
		notes[i] = strings.Join(slices.DeleteFunc([]string{targetNotes[i], sourceNotes[i]}, func(s string) bool { return s == "" }), "; ")
	}
	return notes
}
