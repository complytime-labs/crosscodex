//go:build !integration

package cleaning_test

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

const (
	equivalent = pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT
	subsetOf   = pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF
	supersetOf = pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF
	partial    = pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL
	conflicts  = pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH
)

func ctrl(id, catalog, identifier string) graphdb.Node {
	return graphdb.Node{ID: id, Label: "Control", Properties: map[string]any{
		"identifier": identifier, "label": strings.ToLower(identifier), "title": "Title of " + identifier, "catalog_id": catalog,
	}}
}

// Fixture controls use SP 800-53 (cat-a), SP 800-171r3 (cat-b) and SSDF
// (cat-c) identifiers; the code under test never reads their format.
var (
	a1 = ctrl("a1", "cat-a", "AC-2")
	a2 = ctrl("a2", "cat-a", "AC-3")
	b1 = ctrl("b1", "cat-b", "03.01.01")
	b2 = ctrl("b2", "cat-b", "03.01.02")
	c1 = ctrl("c1", "cat-c", "PO.1.1")

	t1 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 = t1.Add(time.Hour)
	t3 = t2.Add(time.Hour)
)

func key(job string, src, tgt graphdb.Node) cleaning.VerdictKey {
	return cleaning.VerdictKey{JobID: job, SourceID: src.ID, TargetID: tgt.ID}
}

// verdict is job's verdict stored src->tgt; the pair's A side is the first
// node passed to pair.
func verdict(job string, at time.Time, src, tgt graphdb.Node, typ pb.RelationshipType) cleaning.Verdict {
	return cleaning.Verdict{
		Key: key(job, src, tgt), CompletedAt: at, Type: typ,
		EdgeIDs: []string{"e-" + job + "-" + src.ID + "-" + tgt.ID}, Source: src, Target: tgt,
	}
}

func pair(a, b graphdb.Node, vs ...cleaning.Verdict) cleaning.Pair {
	for i := range vs {
		vs[i].Reversed = vs[i].Key.SourceID != a.ID
	}
	return cleaning.Pair{A: a, B: b, Verdicts: vs}
}

func agreement(f float64) *float64 { return &f }

// found is the part of a Finding a signal table asserts.
type found struct{ Concerns, Targets []cleaning.VerdictKey }

func findingsOf(fs []cleaning.Finding, s cleaning.Signal) []found {
	var out []found
	for _, f := range fs {
		if f.Signal == s {
			out = append(out, found{f.Concerns, f.Targets})
		}
	}
	return out
}

func keys(ks ...cleaning.VerdictKey) []cleaning.VerdictKey { return ks }

func policy(minAgreement float64) config.CleaningTenantConfig {
	return config.CleaningTenantConfig{Signals: config.CleaningSignals{
		LowAgreement: config.SignalPolicy{Action: config.CleaningActionReport, MinAgreementFraction: minAgreement},
	}}
}

var _ = Describe("Detect", func() {
	detect := func(pairs []cleaning.Pair, s cleaning.Signal) []found {
		fs, _ := cleaning.Detect(pairs, nil, nil, policy(0.5))
		return findingsOf(fs, s)
	}

	DescribeTable("equivalence_fanout",
		func(pairs []cleaning.Pair, want []found) {
			Expect(detect(pairs, cleaning.SignalEquivalenceFanout)).To(Equal(want))
		},
		Entry("a source equivalent to two targets of one catalog is flagged without targets",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, a1, b1, equivalent)),
				pair(a1, b2, verdict("j1", t1, a1, b2, equivalent)),
			},
			[]found{{Concerns: keys(key("j1", a1, b1), key("j1", a1, b2))}}),
		Entry("a target equivalent to two sources of one catalog is flagged",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, a1, b1, equivalent)),
				pair(a2, b1, verdict("j1", t1, a2, b1, equivalent)),
			},
			[]found{{Concerns: keys(key("j1", a1, b1), key("j1", a2, b1))}}),
		Entry("a reversed stored edge counts and keeps its stored key",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, b1, a1, equivalent)),
				pair(a1, b2, verdict("j1", t1, a1, b2, equivalent)),
			},
			[]found{{Concerns: keys(key("j1", a1, b2), key("j1", b1, a1))}}),
		Entry("only the latest job's verdicts are concerned",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf), verdict("j2", t2, a1, b1, equivalent)),
				pair(a1, b2, verdict("j1", t1, a1, b2, equivalent)),
			},
			[]found{{Concerns: keys(key("j1", a1, b2), key("j2", a1, b1))}}),
		Entry("equivalence to one target only is not flagged",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, equivalent))},
			nil),
		Entry("equivalent targets in different catalogs are not flagged",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, a1, b1, equivalent)),
				pair(a1, c1, verdict("j1", t1, a1, c1, equivalent)),
			},
			nil),
		Entry("an equivalence a later job overrode is not counted",
			[]cleaning.Pair{
				pair(a1, b1, verdict("j1", t1, a1, b1, equivalent), verdict("j2", t2, a1, b1, subsetOf)),
				pair(a1, b2, verdict("j1", t1, a1, b2, equivalent)),
			},
			nil),
	)

	DescribeTable("cross_job_contradiction",
		func(pairs []cleaning.Pair, want []found) {
			Expect(detect(pairs, cleaning.SignalCrossJobContradiction)).To(Equal(want))
		},
		Entry("two jobs that disagree target the older job",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, equivalent), verdict("j2", t2, a1, b1, subsetOf))},
			[]found{{Concerns: keys(key("j1", a1, b1), key("j2", a1, b1)), Targets: keys(key("j1", a1, b1))}}),
		Entry("three jobs target every job but the latest",
			[]cleaning.Pair{pair(a1, b1,
				verdict("j1", t3, a1, b1, partial), verdict("j2", t1, a1, b1, equivalent), verdict("j3", t2, a1, b1, equivalent))},
			[]found{{
				Concerns: keys(key("j1", a1, b1), key("j2", a1, b1), key("j3", a1, b1)),
				Targets:  keys(key("j2", a1, b1), key("j3", a1, b1)),
			}}),
		Entry("equal completion times make the greater job ID the latest",
			[]cleaning.Pair{pair(a1, b1, verdict("j2", t1, a1, b1, equivalent), verdict("j1", t1, a1, b1, partial))},
			[]found{{Concerns: keys(key("j1", a1, b1), key("j2", a1, b1)), Targets: keys(key("j1", a1, b1))}}),
		Entry("a bidirectional job's verdicts are all targeted when another job is later",
			[]cleaning.Pair{pair(a1, b1,
				verdict("j1", t1, a1, b1, equivalent), verdict("j1", t1, b1, a1, equivalent), verdict("j2", t2, a1, b1, partial))},
			[]found{{
				Concerns: keys(key("j1", a1, b1), key("j1", b1, a1), key("j2", a1, b1)),
				Targets:  keys(key("j1", a1, b1), key("j1", b1, a1)),
			}}),
		Entry("two jobs that agree are not flagged",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf), verdict("j2", t2, a1, b1, subsetOf))},
			nil),
		Entry("two jobs that agree once the reversed verdict is inverted are not flagged",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf), verdict("j2", t2, b1, a1, supersetOf))},
			nil),
		Entry("a pair judged by one job is not flagged",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf))},
			nil),
		Entry("a bidirectional job's two disagreeing verdicts are not flagged",
			[]cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, equivalent), verdict("j1", t1, b1, a1, subsetOf))},
			nil),
	)

	DescribeTable("low_agreement",
		func(got *float64, want []found) {
			v := verdict("j1", t1, a1, b1, subsetOf)
			v.Agreement = got
			Expect(detect([]cleaning.Pair{pair(a1, b1, v)}, cleaning.SignalLowAgreement)).To(Equal(want))
		},
		Entry("agreement below the threshold targets the verdict", agreement(0.4),
			[]found{{Concerns: keys(key("j1", a1, b1)), Targets: keys(key("j1", a1, b1))}}),
		Entry("agreement absent is not flagged", nil, nil),
		Entry("agreement equal to the threshold is not flagged", agreement(0.5), nil),
		Entry("agreement above the threshold is not flagged", agreement(0.9), nil),
	)

	It("records the low_agreement threshold in the evidence detail", func() {
		v := verdict("j1", t1, a1, b1, subsetOf)
		v.Agreement = agreement(0.25)
		fs, _ := cleaning.Detect([]cleaning.Pair{pair(a1, b1, v)}, nil, nil, policy(0.375))
		Expect(fs).To(HaveLen(1))
		Expect(fs[0].Evidence.Detail).To(Equal(map[string]string{"min_agreement_fraction": "0.375"}))
	})

	DescribeTable("conflicts_with",
		func(v cleaning.Verdict, a, b graphdb.Node, want []found) {
			Expect(detect([]cleaning.Pair{pair(a, b, v)}, cleaning.SignalConflictsWith)).To(Equal(want))
		},
		Entry("a conflicts-with verdict targets itself", verdict("j1", t1, a1, b1, conflicts), a1, b1,
			[]found{{Concerns: keys(key("j1", a1, b1)), Targets: keys(key("j1", a1, b1))}}),
		Entry("a reversed conflicts-with verdict targets itself", verdict("j1", t1, b1, a1, conflicts), a1, b1,
			[]found{{Concerns: keys(key("j1", b1, a1)), Targets: keys(key("j1", b1, a1))}}),
		Entry("an equivalent verdict is not flagged", verdict("j1", t1, a1, b1, equivalent), a1, b1, nil),
	)

	It("raises the same findings whatever the configured actions", func() {
		pairs := []cleaning.Pair{pair(a1, b1, verdict("j1", t1, a1, b1, conflicts), verdict("j2", t2, a1, b1, subsetOf))}
		report, _ := cleaning.Detect(pairs, nil, nil, policy(0.5))
		retire := policy(0.5)
		retire.Signals.ConflictsWith.Action = config.CleaningActionRetire
		retire.Signals.CrossJobContradiction.Action = config.CleaningActionHuman
		other, _ := cleaning.Detect(pairs, nil, nil, retire)
		Expect(other).To(Equal(report))
		Expect(report).To(HaveLen(2))
	})

	It("fills evidence with readable controls in stored direction", func() {
		v := verdict("j1", t1, b1, a1, conflicts)
		v.Agreement = agreement(0.75)
		v.EdgeIDs = []string{"e1", "e2"}
		jobs := map[string]cleaning.Job{"j1": {ID: "j1", CompletedAt: t1, Catalogs: jobconfig.Catalogs{
			SourceID: "cat-b", TargetID: "cat-a",
			SourceLabel: jobconfig.Label{Name: "SP 800-171", Version: "3.0.0", Recorded: true},
		}}}
		fs, _ := cleaning.Detect([]cleaning.Pair{pair(a1, b1, v)}, jobs, nil, policy(0.5))
		Expect(fs).To(HaveLen(1))
		Expect(fs[0].Evidence.Verdicts).To(Equal([]cleaning.VerdictEvidence{{
			Key: key("j1", b1, a1), JobCompletedAt: t1,
			SourceIdentifier: "03.01.01", SourceLabel: "03.01.01", SourceTitle: "Title of 03.01.01", SourceCatalog: "SP 800-171 3.0.0",
			TargetIdentifier: "AC-2", TargetLabel: "ac-2", TargetTitle: "Title of AC-2", TargetCatalog: "cat-a",
			Relationship: "conflicts with", Agreement: agreement(0.75), EdgeIDs: []string{"e1", "e2"},
		}}))
	})

	It("names the first concern's stored source and target catalogs by ID, not by label", func() {
		jobs := map[string]cleaning.Job{"j1": {ID: "j1", CompletedAt: t1, Catalogs: jobconfig.Catalogs{
			SourceID: "cat-b", TargetID: "cat-a",
			SourceLabel: jobconfig.Label{Name: "SP 800-171", Version: "3.0.0", Recorded: true},
		}}}
		fs, _ := cleaning.Detect([]cleaning.Pair{pair(a1, b1, verdict("j1", t1, b1, a1, conflicts))}, jobs, nil, policy(0.5))
		Expect(fs).To(HaveLen(1))
		Expect([]string{fs[0].SourceCatalog, fs[0].TargetCatalog}).To(Equal([]string{"cat-b", "cat-a"}))
	})

	DescribeTable("excludes verdicts whose key ParseVerdictKey would reject",
		func(bad graphdb.Node) {
			pairs := []cleaning.Pair{
				pair(bad, b1, verdict("j1", t1, bad, b1, conflicts), verdict("j2", t2, bad, b1, equivalent)),
				pair(a1, b1, verdict("j1", t1, a1, b1, conflicts)),
			}
			fs, sum := cleaning.Detect(pairs, nil, nil, policy(0.5))
			Expect(sum).To(Equal(cleaning.Summary{Verdicts: 1, Pairs: 1, UnaddressableVerdicts: 2}))
			Expect(fs).To(HaveLen(1))
			Expect(fs[0].Concerns).To(Equal(keys(key("j1", a1, b1))))
			for _, f := range fs {
				for _, k := range append(slices.Clone(f.Concerns), f.Targets...) {
					Expect(k.SourceID).NotTo(Equal(bad.ID))
				}
			}
		},
		Entry("a source node ID holding ':'", ctrl("cat-a/AC:2", "cat-a", "AC-2")),
		Entry("a 257-byte source node ID", ctrl(strings.Repeat("n", 257), "cat-a", "AC-2")),
	)

	It("counts verdicts and pairs in the summary", func() {
		_, sum := cleaning.Detect([]cleaning.Pair{
			pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf), verdict("j2", t2, a1, b1, subsetOf)),
			pair(a1, b2, verdict("j1", t1, a1, b2, subsetOf)),
		}, nil, nil, policy(0.5))
		Expect(sum).To(Equal(cleaning.Summary{Verdicts: 3, Pairs: 2}))
	})
})

var _ = Describe("Detect staleness", func() {
	rec := func(version string, models map[string]string) *cleaning.StageRecord {
		return &cleaning.StageRecord{PromptName: "p", PromptVersion: version, Models: models}
	}
	pairs := []cleaning.Pair{
		pair(a1, b1, verdict("j1", t1, a1, b1, subsetOf), verdict("j2", t2, a1, b1, subsetOf)),
		pair(a1, b2, verdict("j1", t1, a1, b2, subsetOf)),
	}
	j1Keys := keys(key("j1", a1, b1), key("j1", a1, b2))
	// Only the source catalog's label is recorded, so the target falls back
	// to its catalog ID.
	j1Catalogs := jobconfig.Catalogs{
		SourceID: "cat-a", TargetID: "cat-b",
		SourceLabel: jobconfig.Label{Name: "SP 800-53", Version: "5.1.1", Recorded: true},
	}
	analyzerNow := cleaning.Current{
		results.StageAnalyzer: {PromptName: "p", PromptVersion: "2", Models: map[string]string{"a": "r1", "b": "r2"}},
	}
	bothNow := cleaning.Current{
		results.StageAnalyzer:    analyzerNow[results.StageAnalyzer],
		results.StageSynthesized: {PromptName: "s", PromptVersion: "7", Models: map[string]string{"a": "r1"}},
	}

	type outcome struct {
		Detail  map[string]string // nil: no finding
		Unknown int
	}
	run := func(signal cleaning.Signal, stages map[string]*cleaning.StageRecord, current cleaning.Current) outcome {
		jobs := map[string]cleaning.Job{
			"j1": {ID: "j1", CompletedAt: t1, Stages: stages, Catalogs: j1Catalogs},
			"j2": {ID: "j2", CompletedAt: t2, Stages: map[string]*cleaning.StageRecord{
				results.StageAnalyzer:    rec("2", map[string]string{"a": "r1", "b": "r2"}),
				results.StageSynthesized: rec("7", map[string]string{"a": "r1"}),
			}},
		}
		fs, sum := cleaning.Detect(pairs, jobs, current, policy(0.5))
		var out outcome
		out.Unknown = sum.UnknownPrompt
		if signal == cleaning.SignalStaleModel {
			out.Unknown = sum.UnknownModel
		}
		for _, f := range fs {
			if f.Signal != signal {
				continue
			}
			Expect(out.Detail).To(BeNil(), "one finding per job and signal")
			// Job-level: the evidence names the job, not its verdicts; the
			// targets are the job's live verdicts, which a retire expands to.
			Expect(f.Concerns).To(BeNil())
			Expect(f.Evidence.Verdicts).To(BeNil())
			Expect(f.Evidence.Job).To(Equal(&cleaning.JobEvidence{
				ID: "j1", CompletedAt: t1, SourceCatalog: "SP 800-53 5.1.1", TargetCatalog: "cat-b", VerdictsInScope: 2,
			}))
			Expect(f.Targets).To(Equal(j1Keys))
			Expect(f.TargetEdgeIDs).To(Equal([]string{"e-j1-a1-b1", "e-j1-a1-b2"}))
			Expect([]string{f.SourceCatalog, f.TargetCatalog}).To(Equal([]string{"cat-a", "cat-b"}))
			Expect(cleaning.FindingID(f)).To(Equal(cleaning.FindingID(cleaning.Finding{
				Signal: signal, Evidence: cleaning.Evidence{Job: &cleaning.JobEvidence{ID: "j1"}},
			})))
			out.Detail = f.Evidence.Detail
		}
		return out
	}

	DescribeTable("stale_prompt",
		func(stages map[string]*cleaning.StageRecord, current cleaning.Current, want outcome) {
			Expect(run(cleaning.SignalStalePrompt, stages, current)).To(Equal(want))
		},
		Entry("a changed prompt version targets all of the job's verdicts",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("1", nil)}, analyzerNow,
			outcome{Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "1", "analyzer.current": "2"}}),
		Entry("two stale stages raise one finding naming both",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("1", nil), results.StageSynthesized: rec("6", nil)}, bothNow,
			outcome{Detail: map[string]string{
				"stages":            "analyzer,synthesized",
				"analyzer.recorded": "1", "analyzer.current": "2",
				"synthesized.recorded": "6", "synthesized.current": "7",
			}}),
		Entry("a stale stage beside a nil one is stale and not unknown",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("1", nil), results.StageSynthesized: nil}, bothNow,
			outcome{Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "1", "analyzer.current": "2"}}),
		Entry("the same prompt version is not flagged",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("2", nil)}, analyzerNow, outcome{}),
		Entry("a nil stage record is unknown, not stale",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: nil}, analyzerNow, outcome{Unknown: 1}),
		Entry("an empty recorded prompt version is unknown, not stale",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("", nil)}, analyzerNow, outcome{Unknown: 1}),
		Entry("a stage the job never recorded is unknown",
			map[string]*cleaning.StageRecord{}, analyzerNow, outcome{Unknown: 1}),
		Entry("two unknown stages count the job once",
			map[string]*cleaning.StageRecord{}, bothNow, outcome{Unknown: 1}),
		Entry("a stage absent from current is not compared",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("2", nil), results.StageSynthesized: rec("1", nil)}, analyzerNow,
			outcome{}),
	)

	DescribeTable("stale_model",
		func(stages map[string]*cleaning.StageRecord, want outcome) {
			Expect(run(cleaning.SignalStaleModel, stages, analyzerNow)).To(Equal(want))
		},
		Entry("a changed revision targets all of the job's verdicts",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("2", map[string]string{"b": "r9", "a": "r1"})},
			outcome{Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "a=r1,b=r9", "analyzer.current": "a=r1,b=r2"}}),
		Entry("a changed alias set is flagged",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("2", map[string]string{"a": "r1"})},
			outcome{Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "a=r1", "analyzer.current": "a=r1,b=r2"}}),
		Entry("a revision unknown on one side is not flagged",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: rec("2", map[string]string{"a": "", "b": "r2"})}, outcome{}),
		Entry("a nil stage record is unknown, not stale",
			map[string]*cleaning.StageRecord{results.StageAnalyzer: nil}, outcome{Unknown: 1}),
	)
})

var _ = Describe("BuildPairs", func() {
	edge := func(job string, src, tgt graphdb.Node, reversed bool, stage, id string, typ pb.RelationshipType, props map[string]any) crosswalk.EdgeVerdict {
		p := map[string]any{"job_id": job, results.StageProperty: stage}
		for k, v := range props {
			p[k] = v
		}
		return crosswalk.EdgeVerdict{
			Verdict:  crosswalk.Verdict{JobID: job, CompletedAt: t1, Type: typ, Confidence: 0.8, Reversed: reversed},
			Edge:     graphdb.Edge{ID: id, Properties: p},
			SourceID: src.ID, TargetID: tgt.ID,
		}
	}
	pe := func(a, b graphdb.Node, vs ...crosswalk.EdgeVerdict) crosswalk.PairEdges {
		return crosswalk.PairEdges{A: a, B: b, Verdicts: vs}
	}

	It("merges a verdict's analyzer and synthesized edges under one key", func() {
		pairs := cleaning.BuildPairs(
			[]crosswalk.PairEdges{pe(a1, b1, edge("j1", b1, a1, true, results.StageAnalyzer, "e9", equivalent, map[string]any{"agreement_fraction": "0.75"}))},
			[]crosswalk.PairEdges{pe(a1, b1, edge("j1", b1, a1, true, results.StageSynthesized, "e1", subsetOf, nil))},
		)
		Expect(pairs).To(HaveLen(1))
		Expect(pairs[0].A.ID).To(Equal("a1"))
		Expect(pairs[0].B.ID).To(Equal("b1"))
		Expect(pairs[0].Verdicts).To(HaveLen(1))
		v := pairs[0].Verdicts[0]
		Expect(v.Key).To(Equal(key("j1", b1, a1)))
		Expect(v.EdgeIDs).To(Equal([]string{"e1", "e9"}))
		Expect(v.Type).To(Equal(equivalent))
		Expect(v.Reversed).To(BeTrue())
		Expect(v.CompletedAt).To(Equal(t1))
		Expect(v.Agreement).To(Equal(agreement(0.75)))
		Expect(v.Source.ID).To(Equal("b1"))
		Expect(v.Target.ID).To(Equal("a1"))
	})

	It("takes the synthesized type when the analyzer edge is absent, and orders pairs and verdicts", func() {
		pairs := cleaning.BuildPairs(
			[]crosswalk.PairEdges{pe(a2, b1, edge("j2", a2, b1, false, results.StageAnalyzer, "e3", partial, nil))},
			[]crosswalk.PairEdges{
				pe(a2, b1, edge("j1", a2, b1, false, results.StageSynthesized, "e4", conflicts, nil)),
				pe(a1, b1, edge("j1", a1, b1, false, results.StageSynthesized, "e5", subsetOf, nil)),
			},
		)
		Expect(pairs).To(HaveLen(2))
		Expect(pairs[0].A.ID).To(Equal("a1"))
		Expect(pairs[0].Verdicts[0].Type).To(Equal(subsetOf))
		Expect(pairs[1].Verdicts).To(HaveLen(2))
		Expect(pairs[1].Verdicts[0].Key.JobID).To(Equal("j1"))
		Expect(pairs[1].Verdicts[0].Type).To(Equal(conflicts))
		Expect(pairs[1].Verdicts[1].Type).To(Equal(partial))
	})

	DescribeTable("reads agreement_fraction",
		func(value any, want *float64) {
			pairs := cleaning.BuildPairs([]crosswalk.PairEdges{pe(a1, b1,
				edge("j1", a1, b1, false, results.StageAnalyzer, "e1", subsetOf, map[string]any{"agreement_fraction": value}))}, nil)
			Expect(pairs[0].Verdicts[0].Agreement).To(Equal(want))
		},
		Entry("a decimal string", "0.667", agreement(0.667)),
		Entry("a stored number", 0.5, agreement(0.5)),
		Entry("unparsable text as nil", "high", nil),
		Entry("NaN as nil", "NaN", nil),
		Entry("infinity as nil", "+Inf", nil),
		Entry("an absent property as nil", nil, nil),
	)
})

var _ = Describe("VerdictKey", func() {
	It("round-trips through String and ParseVerdictKey", func() {
		k := cleaning.VerdictKey{JobID: "job-1", SourceID: "844424930131969", TargetID: "844424930131970"}
		Expect(k.String()).To(Equal("job-1:844424930131969:844424930131970"))
		got, err := cleaning.ParseVerdictKey(k.String())
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(k))
	})

	It("accepts parts of exactly 256 bytes", func() {
		s := strings.Repeat("j", 256) + ":" + strings.Repeat("s", 256) + ":" + strings.Repeat("t", 256)
		got, err := cleaning.ParseVerdictKey(s)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.String()).To(Equal(s))
	})

	DescribeTable("rejects malformed keys",
		func(s, wantMsg string) {
			_, err := cleaning.ParseVerdictKey(s)
			Expect(err).To(MatchError(cleaning.ErrInvalidVerdictKey))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
		},
		Entry("two parts", "job:src", "<job>:<source>:<target>"),
		Entry("four parts", "job:src:tgt:x", "<job>:<source>:<target>"),
		Entry("an empty part", "job::tgt", "empty"),
		Entry("the empty string", "", "<job>:<source>:<target>"),
		Entry("a 257-byte part", "job:"+strings.Repeat("s", 257)+":tgt", "256 bytes"),
	)
})

var _ = Describe("ParseEvidence", func() {
	full := cleaning.Evidence{
		Verdicts: []cleaning.VerdictEvidence{{
			Key: key("j1", a1, b1), JobCompletedAt: t1,
			SourceIdentifier: "AC-2", SourceLabel: "ac-2", SourceTitle: "Account Management", SourceCatalog: "SP 800-53 5.1.1",
			TargetIdentifier: "03.01.01", TargetLabel: "03.01.01", TargetTitle: "Account Management", TargetCatalog: "cat-b",
			Relationship: "subset of", Agreement: agreement(0.5), EdgeIDs: []string{"e1", "e2"},
		}},
		Detail: map[string]string{"min_agreement_fraction": "0.6"},
	}

	It("round-trips stored evidence", func() {
		b, err := json.Marshal(full)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring(`"source_identifier":"AC-2"`))
		Expect(string(b)).To(ContainSubstring(`"key":{"job_id":"j1","source_id":"a1","target_id":"b1"}`))
		got, err := cleaning.ParseEvidence(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(full))
	})

	It("round-trips a job-level finding's evidence", func() {
		job := cleaning.Evidence{
			Job:    &cleaning.JobEvidence{ID: "j1", CompletedAt: t1, SourceCatalog: "SP 800-53 5.1.1", TargetCatalog: "cat-b", VerdictsInScope: 2000},
			Detail: map[string]string{"stages": "analyzer"},
		}
		b, err := json.Marshal(job)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring(`"job":{"id":"j1","completed_at":"2026-01-01T00:00:00Z","source_catalog":"SP 800-53 5.1.1","target_catalog":"cat-b","verdicts_in_scope":2000}`))
		got, err := cleaning.ParseEvidence(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(job))
	})

	DescribeTable("rejects malformed evidence",
		func(b []byte, wantMsg string) {
			_, err := cleaning.ParseEvidence(b)
			Expect(err).To(MatchError(cleaning.ErrInvalidEvidence))
			Expect(err.Error()).To(ContainSubstring(wantMsg))
		},
		Entry("an unknown field", []byte(`{"verdicts":[],"extra":1}`), "unknown field"),
		Entry("oversized input", []byte(`{"verdicts":[],"detail":{"x":"`+strings.Repeat("x", 1<<20)+`"}}`), "exceeds"),
		Entry("trailing data", []byte(`{"verdicts":[]} {}`), "after the evidence"),
		Entry("a verdict key with an empty part", []byte(`{"verdicts":[{"key":{"job_id":"","source_id":"s","target_id":"t"}}]}`), "empty"),
		Entry("a verdict key part holding the separator", []byte(`{"verdicts":[{"key":{"job_id":"a:b","source_id":"s","target_id":"t"}}]}`), "':'"),
		Entry("an empty job ID", []byte(`{"verdicts":null,"job":{"id":""}}`), "job part is empty"),
		Entry("a job ID holding the separator", []byte(`{"verdicts":null,"job":{"id":"a:b"}}`), "job part contains the separator"),
	)
})

var _ = Describe("Signal and Status values", func() {
	DescribeTable("Signal.JobLevel is true for the staleness signals only",
		func(s cleaning.Signal, want bool) { Expect(s.JobLevel()).To(Equal(want)) },
		Entry("equivalence_fanout", cleaning.SignalEquivalenceFanout, false),
		Entry("cross_job_contradiction", cleaning.SignalCrossJobContradiction, false),
		Entry("low_agreement", cleaning.SignalLowAgreement, false),
		Entry("conflicts_with", cleaning.SignalConflictsWith, false),
		Entry("stale_prompt", cleaning.SignalStalePrompt, true),
		Entry("stale_model", cleaning.SignalStaleModel, true),
	)

	DescribeTable("Signal.Valid accepts exactly the listed signals",
		func(s cleaning.Signal, want bool) {
			Expect(s.Valid()).To(Equal(want))
			Expect(slices.Contains(cleaning.Signals(), s)).To(Equal(want))
		},
		Entry("equivalence_fanout", cleaning.SignalEquivalenceFanout, true),
		Entry("cross_job_contradiction", cleaning.SignalCrossJobContradiction, true),
		Entry("low_agreement", cleaning.SignalLowAgreement, true),
		Entry("conflicts_with", cleaning.SignalConflictsWith, true),
		Entry("stale_prompt", cleaning.SignalStalePrompt, true),
		Entry("stale_model", cleaning.SignalStaleModel, true),
		Entry("empty", cleaning.Signal(""), false),
		Entry("wrong case", cleaning.Signal("Low_Agreement"), false),
		Entry("unknown", cleaning.Signal("low_agreemnt"), false),
	)

	DescribeTable("Status.Valid accepts exactly the listed statuses",
		func(s cleaning.Status, want bool) {
			Expect(s.Valid()).To(Equal(want))
			Expect(slices.Contains(cleaning.Statuses(), s)).To(Equal(want))
		},
		Entry("reported", cleaning.StatusReported, true),
		Entry("pending", cleaning.StatusPending, true),
		Entry("retired", cleaning.StatusRetired, true),
		Entry("dismissed", cleaning.StatusDismissed, true),
		Entry("restored", cleaning.StatusRestored, true),
		Entry("cleared", cleaning.StatusCleared, true),
		Entry("empty", cleaning.Status(""), false),
		Entry("unknown", cleaning.Status("retierd"), false),
	)

	It("returns copies callers cannot use to change the lists", func() {
		cleaning.Signals()[0] = "x"
		cleaning.Statuses()[0] = "x"
		Expect(cleaning.Signal("x").Valid()).To(BeFalse())
		Expect(cleaning.Status("x").Valid()).To(BeFalse())
	})
})
