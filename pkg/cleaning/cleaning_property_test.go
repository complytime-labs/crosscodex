//go:build !integration

package cleaning_test

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

// Suite bootstrap lives in cleaning_suite_test.go.

var _ = Describe("Property Specifications", Ordered, func() {
	drawRecord := func(t *rapid.T) cleaning.StageRecord {
		return cleaning.StageRecord{
			PromptName:    rapid.String().Draw(t, "name"),
			PromptVersion: rapid.String().Draw(t, "version"),
			Models: rapid.MapOfN(
				rapid.StringN(0, 40, 256), rapid.StringN(0, 40, 256), 0, 8,
			).Draw(t, "models"),
		}
	}

	It("Marshal then ParseStageRecord returns an equal record", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			r := drawRecord(t)
			s, err := r.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := cleaning.ParseStageRecord(s)
			if err != nil {
				t.Fatalf("ParseStageRecord(%q): %v", s, err)
			}
			if got == nil || !equalRecords(*got, r) {
				t.Fatalf("round trip changed record: %+v -> %+v", r, got)
			}
		})
	})

	It("Marshal is deterministic", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			r := drawRecord(t)
			a, errA := r.Marshal()
			b, errB := r.Marshal()
			if errA != nil || errB != nil || a != b {
				t.Fatalf("Marshal not deterministic: %q (%v) vs %q (%v)", a, errA, b, errB)
			}
		})
	})

	It("a record is never stale against itself", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			r := drawRecord(t)
			if cleaning.PromptStale(&r, r) || cleaning.ModelStale(&r, r) {
				t.Fatalf("record stale against itself: %+v", r)
			}
		})
	})

	It("a nil recorded stage is never stale", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			r := drawRecord(t)
			if cleaning.PromptStale(nil, r) || cleaning.ModelStale(nil, r) {
				t.Fatalf("unknown record reported stale against %+v", r)
			}
		})
	})

	It("ParseVerdictKey inverts VerdictKey.String", func() {
		part := rapid.StringMatching(`[A-Za-z0-9_.-]{1,256}`)
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			k := cleaning.VerdictKey{JobID: part.Draw(t, "job"), SourceID: part.Draw(t, "source"), TargetID: part.Draw(t, "target")}
			got, err := cleaning.ParseVerdictKey(k.String())
			if err != nil || got != k {
				t.Fatalf("ParseVerdictKey(%q) = %+v, %v", k.String(), got, err)
			}
		})
	})

	It("Detect targets only concerned verdicts, sorted, whatever the pair order", func() {
		types := []pb.RelationshipType{equivalent, subsetOf, supersetOf, partial, conflicts}
		times := []time.Time{t1, t2, t3}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			var pairs []cleaning.Pair
			for _, ends := range [][2]graphdb.Node{{a1, b1}, {a1, b2}, {a2, b1}, {a2, b2}} {
				var vs []cleaning.Verdict
				for j, job := range []string{"j1", "j2", "j3"} {
					for _, reversed := range []bool{false, true} {
						if !rapid.Bool().Draw(t, "judged") {
							continue
						}
						src, tgt := ends[0], ends[1]
						if reversed {
							src, tgt = tgt, src
						}
						v := verdict(job, times[j], src, tgt, rapid.SampledFrom(types).Draw(t, "type"))
						if rapid.Bool().Draw(t, "has agreement") {
							v.Agreement = agreement(rapid.Float64Range(0, 1).Draw(t, "agreement"))
						}
						vs = append(vs, v)
					}
				}
				if len(vs) > 0 {
					pairs = append(pairs, pair(ends[0], ends[1], vs...))
				}
			}
			fs, sum := cleaning.Detect(pairs, nil, nil, policy(0.5))
			for _, f := range fs {
				if len(f.Concerns) == 0 || !slices.IsSortedFunc(f.Concerns, compareVerdictKeys) || !slices.IsSortedFunc(f.Targets, compareVerdictKeys) {
					t.Fatalf("%s finding has empty or unsorted keys: %+v", f.Signal, f)
				}
				for _, k := range f.Targets {
					if !slices.Contains(f.Concerns, k) {
						t.Fatalf("%s target %s is not a concern", f.Signal, k)
					}
				}
			}
			reversed := slices.Clone(pairs)
			slices.Reverse(reversed)
			again, sumAgain := cleaning.Detect(reversed, nil, nil, policy(0.5))
			if !reflect.DeepEqual(fs, again) || sum != sumAgain {
				t.Fatalf("Detect depends on pair order:\n%+v\n%+v", fs, again)
			}
		})
	})

	verdictPool := []cleaning.VerdictKey{
		key("j1", a1, b1), key("j2", a1, b1), key("j1", a1, b2), key("j3", a2, b1), key("j2", b2, a2),
	}
	actions := []string{config.CleaningActionReport, config.CleaningActionHuman, config.CleaningActionRetire}
	statuses := []cleaning.Status{
		cleaning.StatusReported, cleaning.StatusPending, cleaning.StatusRetired,
		cleaning.StatusDismissed, cleaning.StatusRestored, cleaning.StatusCleared,
	}
	catalogs := []string{"cat-a", "cat-b", "cat-c"}

	// drawPlanInputs draws findings with distinct IDs, stored rows for some of
	// them and for undetected ones, a policy that never retires
	// equivalence_fanout, and a scope.
	drawPlanInputs := func(t *rapid.T) ([]cleaning.Finding, []cleaning.StoredFinding, config.CleaningTenantConfig, cleaning.Scope) {
		signals := []cleaning.Signal{
			cleaning.SignalEquivalenceFanout, cleaning.SignalCrossJobContradiction, cleaning.SignalLowAgreement,
			cleaning.SignalConflictsWith, cleaning.SignalStalePrompt, cleaning.SignalStaleModel,
		}
		drawFinding := func(t *rapid.T) cleaning.Finding {
			s := rapid.SampledFrom(signals).Draw(t, "signal")
			if s.JobLevel() {
				job := rapid.SampledFrom([]string{"j1", "j2", "j3"}).Draw(t, "job")
				var live []cleaning.VerdictKey
				for _, k := range verdictPool {
					if k.JobID == job {
						live = append(live, k)
					}
				}
				slices.SortFunc(live, compareVerdictKeys)
				return jobFinding(s, job, live)
			}
			concerns := rapid.SliceOfNDistinct(rapid.SampledFrom(verdictPool), 1, len(verdictPool), func(k cleaning.VerdictKey) string { return k.String() }).Draw(t, "concerns")
			slices.SortFunc(concerns, compareVerdictKeys)
			var targets []cleaning.VerdictKey
			if s != cleaning.SignalEquivalenceFanout {
				for _, k := range concerns {
					if rapid.Bool().Draw(t, "target") {
						targets = append(targets, k)
					}
				}
				if len(targets) == 0 {
					targets = concerns[:1]
				}
			}
			f := planFinding(s, concerns, targets)
			f.SourceCatalog, f.TargetCatalog = rapid.SampledFrom(catalogs).Draw(t, "source catalog"), rapid.SampledFrom(catalogs).Draw(t, "target catalog")
			return f
		}
		all := rapid.SliceOfNDistinct(rapid.Custom(drawFinding), 0, 8, cleaning.FindingID).Draw(t, "findings")

		var found []cleaning.Finding
		var stored []cleaning.StoredFinding
		for _, f := range all {
			detected := rapid.Bool().Draw(t, "detected")
			if detected {
				found = append(found, f)
			}
			if !detected || rapid.Bool().Draw(t, "stored") {
				row, err := storedRow(f, rapid.SampledFrom(statuses).Draw(t, "status"))
				if err != nil {
					t.Fatalf("EvidenceHash: %v", err)
				}
				if rapid.Bool().Draw(t, "evidence changed") {
					row.EvidenceHash = "evidence-before"
				}
				stored = append(stored, row)
			}
		}

		pol := config.CleaningTenantConfig{MaxRetirePerRun: 1000}
		for _, p := range []*config.SignalPolicy{
			&pol.Signals.CrossJobContradiction, &pol.Signals.LowAgreement, &pol.Signals.ConflictsWith,
			&pol.Signals.StalePrompt, &pol.Signals.StaleModel,
		} {
			p.Action = rapid.SampledFrom(actions).Draw(t, "action")
		}
		pol.Signals.EquivalenceFanout.Action = rapid.SampledFrom(actions[:2]).Draw(t, "fanout action")
		pol.Signals.LowAgreement.MinAgreementFraction = 0.5

		var scope cleaning.Scope
		if rapid.Bool().Draw(t, "scoped") {
			scope = cleaning.Scope{SourceCatalog: rapid.SampledFrom(catalogs).Draw(t, "scope source"), TargetCatalog: rapid.SampledFrom(catalogs).Draw(t, "scope target")}
		}
		return found, stored, pol, scope
	}

	It("BuildPlan and its token do not depend on input order", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			found, stored, pol, scope := drawPlanInputs(t)
			plan, err := cleaning.BuildPlan(found, stored, pol, scope, cleaning.Summary{})
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			again, err := cleaning.BuildPlan(
				rapid.Permutation(found).Draw(t, "found order"), rapid.Permutation(stored).Draw(t, "stored order"),
				pol, scope, cleaning.Summary{})
			if err != nil {
				t.Fatalf("BuildPlan on permuted input: %v", err)
			}
			if plan.Token != again.Token || !reflect.DeepEqual(plan, again) {
				t.Fatalf("plan depends on input order:\n%+v\n%+v", plan, again)
			}
		})
	})

	It("BuildPlan retires each verdict and edge at most once, in sorted order", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			found, stored, pol, scope := drawPlanInputs(t)
			plan, err := cleaning.BuildPlan(found, stored, pol, scope, cleaning.Summary{})
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			if !slices.IsSortedFunc(plan.Retire, compareVerdictKeys) || len(slices.Compact(slices.Clone(plan.Retire))) != len(plan.Retire) {
				t.Fatalf("Retire is unsorted or has duplicates: %v", plan.Retire)
			}
			if !slices.IsSorted(plan.RetireEdgeIDs) || len(slices.Compact(slices.Clone(plan.RetireEdgeIDs))) != len(plan.RetireEdgeIDs) {
				t.Fatalf("RetireEdgeIDs is unsorted or has duplicates: %v", plan.RetireEdgeIDs)
			}
		})
	})

	It("FindingID does not depend on Concerns order", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			concerns := rapid.SliceOfNDistinct(rapid.SampledFrom(verdictPool), 1, len(verdictPool), func(k cleaning.VerdictKey) string { return k.String() }).Draw(t, "concerns")
			f := cleaning.Finding{Signal: cleaning.SignalStaleModel, Concerns: concerns}
			g := cleaning.Finding{Signal: cleaning.SignalStaleModel, Concerns: rapid.Permutation(concerns).Draw(t, "order")}
			if cleaning.FindingID(f) != cleaning.FindingID(g) {
				t.Fatalf("FindingID depends on order: %v vs %v", f.Concerns, g.Concerns)
			}
		})
	})

	It("FindingID of a job-level finding depends on neither the order nor the set of the job's live verdicts", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			signal := rapid.SampledFrom([]cleaning.Signal{cleaning.SignalStalePrompt, cleaning.SignalStaleModel}).Draw(t, "signal")
			job := rapid.SampledFrom([]string{"j1", "j2", "j3"}).Draw(t, "job")
			live := rapid.SliceOfDistinct(rapid.SampledFrom(verdictPool), func(k cleaning.VerdictKey) string { return k.String() }).Draw(t, "live")
			other := rapid.SliceOfDistinct(rapid.SampledFrom(verdictPool), func(k cleaning.VerdictKey) string { return k.String() }).Draw(t, "other live")
			f := jobFinding(signal, job, live)
			g := jobFinding(signal, job, rapid.Permutation(other).Draw(t, "order"))
			want := cleaning.FindingID(cleaning.Finding{Signal: signal, Evidence: cleaning.Evidence{Job: &cleaning.JobEvidence{ID: job}}})
			if cleaning.FindingID(f) != want || cleaning.FindingID(g) != want {
				t.Fatalf("FindingID depends on the live verdicts: %s, %s, want %s", cleaning.FindingID(f), cleaning.FindingID(g), want)
			}
			hf, err := cleaning.EvidenceHash(f.Evidence)
			if err != nil {
				t.Fatalf("EvidenceHash: %v", err)
			}
			hg, err := cleaning.EvidenceHash(g.Evidence)
			if err != nil {
				t.Fatalf("EvidenceHash: %v", err)
			}
			if hf != hg {
				t.Fatalf("EvidenceHash depends on the live verdicts: %s vs %s", hf, hg)
			}
		})
	})

	It("retire then restore leaves the live (endpoints, label, type) set equal to the original", func() {
		ctx := context.Background()
		types := []string{"SUBSET_OF", "EQUIVALENT", "SUPERSET_OF", "RELATED"}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			g := memdriver.New()
			if err := g.CreateGraph(ctx, "tenant-prop"); err != nil {
				t.Fatalf("CreateGraph: %v", err)
			}
			for _, id := range []string{"a1", "a2", "b1", "b2"} {
				if err := g.CreateNode(ctx, "tenant-prop", graphdb.Node{ID: id, Label: "Control", ValidFrom: t1}); err != nil {
					t.Fatalf("CreateNode: %v", err)
				}
			}
			n := rapid.IntRange(1, 6).Draw(t, "edges")
			var ids []string
			want := map[string]int{}
			for i := range n {
				src := rapid.SampledFrom([]string{"a1", "a2"}).Draw(t, "src")
				tgt := rapid.SampledFrom([]string{"b1", "b2"}).Draw(t, "tgt")
				typ := rapid.SampledFrom(types).Draw(t, "type")
				id := "e" + strconv.Itoa(i)
				e := graphdb.Edge{ID: id, Label: "SEMANTIC_MATCH", ValidFrom: t1, Properties: map[string]any{"relationship_type": typ}}
				if err := g.CreateEdge(ctx, "tenant-prop", src, tgt, e); err != nil {
					t.Fatalf("CreateEdge: %v", err)
				}
				ids = append(ids, id)
				want[src+">"+tgt+":"+typ]++
			}
			if _, err := cleaning.RetireEdges(ctx, g, "tenant-prop", ids, t2); err != nil {
				t.Fatalf("RetireEdges: %v", err)
			}
			if _, err := cleaning.RestoreEdges(ctx, g, "tenant-prop", ids, "dec", t3); err != nil {
				t.Fatalf("RestoreEdges: %v", err)
			}
			if _, err := cleaning.RestoreEdges(ctx, g, "tenant-prop", ids, "dec-2", t3.Add(time.Hour)); err != nil {
				t.Fatalf("second RestoreEdges: %v", err)
			}
			rels, err := g.QueryRelationships(ctx, "tenant-prop", graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
			if err != nil {
				t.Fatalf("QueryRelationships: %v", err)
			}
			got := map[string]int{}
			for _, r := range rels {
				typ, _ := r.Edge.Properties["relationship_type"].(string)
				got[r.Source.ID+">"+r.Target.ID+":"+typ]++
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("live set after retire+restore = %v, want %v", got, want)
			}
		})
	})
})

func compareVerdictKeys(x, y cleaning.VerdictKey) int {
	return strings.Compare(x.JobID+"\x00"+x.SourceID+"\x00"+x.TargetID, y.JobID+"\x00"+y.SourceID+"\x00"+y.TargetID)
}

func equalRecords(a, b cleaning.StageRecord) bool {
	if a.PromptName != b.PromptName || a.PromptVersion != b.PromptVersion || len(a.Models) != len(b.Models) {
		return false
	}
	for k, v := range a.Models {
		if bv, ok := b.Models[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
