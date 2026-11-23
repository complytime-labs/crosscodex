//go:build !integration

package cleaning_test

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// planPolicy sets every signal to action, with a cap that never binds.
func planPolicy(action string) config.CleaningTenantConfig {
	p := config.SignalPolicy{Action: action}
	return config.CleaningTenantConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
		EquivalenceFanout: config.SignalPolicy{Action: config.CleaningActionReport}, CrossJobContradiction: p,
		LowAgreement: config.SignalPolicy{Action: action, MinAgreementFraction: 0.5}, ConflictsWith: p, StalePrompt: p, StaleModel: p,
	}}
}

// planFinding is a finding on verdicts stored cat-a -> cat-b whose edges are
// "e-<job>-<source>-<target>" and "s-<job>-<source>-<target>".
func planFinding(s cleaning.Signal, concerns, targets []cleaning.VerdictKey) cleaning.Finding {
	f := cleaning.Finding{Signal: s, Concerns: concerns, Targets: targets, SourceCatalog: "cat-a", TargetCatalog: "cat-b"}
	for _, k := range concerns {
		id := k.JobID + "-" + k.SourceID + "-" + k.TargetID
		f.Evidence.Verdicts = append(f.Evidence.Verdicts, cleaning.VerdictEvidence{
			Key: k, JobCompletedAt: t1, SourceIdentifier: k.SourceID, TargetIdentifier: k.TargetID,
			Relationship: "conflicts with", EdgeIDs: []string{"e-" + id, "s-" + id},
		})
	}
	return f
}

// storedRow is f as the store would hold it with status; property specs use
// it directly because they may not call Gomega.
func storedRow(f cleaning.Finding, status cleaning.Status) (cleaning.StoredFinding, error) {
	h, err := cleaning.EvidenceHash(f.Evidence)
	return cleaning.StoredFinding{
		ID: cleaning.FindingID(f), Signal: f.Signal, Evidence: f.Evidence, EvidenceHash: h,
		Concerns: f.Concerns, Targets: f.Targets, Status: status,
		SourceCatalog: f.SourceCatalog, TargetCatalog: f.TargetCatalog, UpdatedAt: t1,
	}, err
}

func storedOf(f cleaning.Finding, status cleaning.Status) cleaning.StoredFinding {
	row, err := storedRow(f, status)
	Expect(err).NotTo(HaveOccurred())
	return row
}

// jobFinding is a job-level finding of signal s on job, as Detect raises it:
// the analyzer stage's prompt went from 1 to 2, and targets are the job's live
// verdicts stored cat-a -> cat-b with planFinding's edge names.
func jobFinding(s cleaning.Signal, job string, targets []cleaning.VerdictKey) cleaning.Finding {
	f := cleaning.Finding{Signal: s, Targets: targets, SourceCatalog: "cat-a", TargetCatalog: "cat-b", Evidence: cleaning.Evidence{
		Job: &cleaning.JobEvidence{
			ID: job, CompletedAt: t1, SourceCatalog: "SP 800-53 5.1.1", TargetCatalog: "cat-b", VerdictsInScope: len(targets),
		},
		Detail: map[string]string{"stages": "analyzer", "analyzer.recorded": "1", "analyzer.current": "2"},
	}}
	for _, k := range targets {
		id := k.JobID + "-" + k.SourceID + "-" + k.TargetID
		f.TargetEdgeIDs = append(f.TargetEdgeIDs, "e-"+id, "s-"+id)
	}
	slices.Sort(f.TargetEdgeIDs)
	return f
}

var _ = Describe("BuildPlan", func() {
	conflict := planFinding(cleaning.SignalConflictsWith, keys(key("j1", a1, b1)), keys(key("j1", a1, b1)))

	// stored describes the stored state of conflict; nil means not stored.
	type stored struct {
		Status       cleaning.Status
		SameEvidence bool
	}
	// outcome is what the plan says about conflict; nil means omitted.
	type outcome struct {
		Change   cleaning.Change
		From, To cleaning.Status
	}

	DescribeTable("plans each finding from its stored state and the policy",
		func(action string, st *stored, detected bool, scope cleaning.Scope, want *outcome) {
			var found []cleaning.Finding
			if detected {
				found = append(found, conflict)
			}
			var rows []cleaning.StoredFinding
			if st != nil {
				row := storedOf(conflict, st.Status)
				if !st.SameEvidence {
					row.EvidenceHash = "evidence-before"
				}
				rows = append(rows, row)
			}
			plan, err := cleaning.BuildPlan(found, rows, planPolicy(action), scope, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
			if want == nil {
				Expect(plan.Findings).To(BeEmpty())
				return
			}
			Expect(plan.Findings).To(HaveLen(1))
			got := plan.Findings[0]
			Expect(outcome{got.Change, got.From, got.To}).To(Equal(*want))
			Expect(got.ID).To(Equal(cleaning.FindingID(conflict)))
			Expect(got.Action).To(Equal(cleaning.Action(action)))
		},
		Entry("an unstored report finding is new and reported",
			config.CleaningActionReport, nil, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, "", cleaning.StatusReported}),
		Entry("an unstored human finding is new and pending",
			config.CleaningActionHuman, nil, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, "", cleaning.StatusPending}),
		Entry("an unstored retire finding is new and retired",
			config.CleaningActionRetire, nil, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, "", cleaning.StatusRetired}),
		Entry("a stored cleared finding detected again is new",
			config.CleaningActionReport, &stored{cleaning.StatusCleared, true}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, cleaning.StatusCleared, cleaning.StatusReported}),
		Entry("a stored stale finding detected again with the same evidence is new",
			config.CleaningActionRetire, &stored{cleaning.StatusStale, true}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, cleaning.StatusStale, cleaning.StatusRetired}),
		Entry("a stored stale finding detected again with changed evidence is new",
			config.CleaningActionHuman, &stored{cleaning.StatusStale, false}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeNew, cleaning.StatusStale, cleaning.StatusPending}),
		Entry("a stored reported finding with changed evidence is changed",
			config.CleaningActionReport, &stored{cleaning.StatusReported, false}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeChanged, cleaning.StatusReported, cleaning.StatusReported}),
		Entry("a stored pending finding with changed evidence takes the desired status",
			config.CleaningActionRetire, &stored{cleaning.StatusPending, false}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeChanged, cleaning.StatusPending, cleaning.StatusRetired}),
		Entry("a stored pending finding whose action became report changes status",
			config.CleaningActionReport, &stored{cleaning.StatusPending, true}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeStatus, cleaning.StatusPending, cleaning.StatusReported}),
		Entry("a stored reported finding whose action became human changes status",
			config.CleaningActionHuman, &stored{cleaning.StatusReported, true}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeStatus, cleaning.StatusReported, cleaning.StatusPending}),
		Entry("a stored reported finding with the same evidence and status is omitted",
			config.CleaningActionReport, &stored{cleaning.StatusReported, true}, true, cleaning.Scope{}, nil),
		Entry("a stored pending finding with the same evidence and status is omitted",
			config.CleaningActionHuman, &stored{cleaning.StatusPending, true}, true, cleaning.Scope{}, nil),
		Entry("a stored dismissed finding with changed evidence is reraised",
			config.CleaningActionHuman, &stored{cleaning.StatusDismissed, false}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeReraised, cleaning.StatusDismissed, cleaning.StatusPending}),
		Entry("a stored dismissed finding with the same evidence is omitted",
			config.CleaningActionRetire, &stored{cleaning.StatusDismissed, true}, true, cleaning.Scope{}, nil),
		Entry("a stored restored finding with changed evidence is reraised",
			config.CleaningActionReport, &stored{cleaning.StatusRestored, false}, true, cleaning.Scope{},
			&outcome{cleaning.ChangeReraised, cleaning.StatusRestored, cleaning.StatusReported}),
		Entry("a stored restored finding with the same evidence is omitted",
			config.CleaningActionRetire, &stored{cleaning.StatusRestored, true}, true, cleaning.Scope{}, nil),
		Entry("a stored retired finding with changed evidence is omitted",
			config.CleaningActionRetire, &stored{cleaning.StatusRetired, false}, true, cleaning.Scope{}, nil),
		Entry("a stored retired finding no longer detected is omitted, not cleared",
			config.CleaningActionReport, &stored{cleaning.StatusRetired, true}, false, cleaning.Scope{}, nil),
		Entry("a stored dismissed finding no longer detected is omitted",
			config.CleaningActionReport, &stored{cleaning.StatusDismissed, true}, false, cleaning.Scope{}, nil),
		Entry("a stored cleared finding no longer detected is omitted",
			config.CleaningActionReport, &stored{cleaning.StatusCleared, true}, false, cleaning.Scope{}, nil),
		Entry("a stored stale finding no longer detected is omitted",
			config.CleaningActionReport, &stored{cleaning.StatusStale, true}, false, cleaning.Scope{}, nil),
		Entry("a stored reported finding no longer detected under an empty scope is cleared",
			config.CleaningActionReport, &stored{cleaning.StatusReported, true}, false, cleaning.Scope{},
			&outcome{cleaning.ChangeCleared, cleaning.StatusReported, cleaning.StatusCleared}),
		Entry("a stored pending finding no longer detected in its own scope is cleared",
			config.CleaningActionHuman, &stored{cleaning.StatusPending, true}, false, cleaning.Scope{SourceCatalog: "cat-a", TargetCatalog: "cat-b"},
			&outcome{cleaning.ChangeCleared, cleaning.StatusPending, cleaning.StatusCleared}),
		Entry("a stored reported finding no longer detected in the reversed scope is cleared",
			config.CleaningActionReport, &stored{cleaning.StatusReported, true}, false, cleaning.Scope{SourceCatalog: "cat-b", TargetCatalog: "cat-a"},
			&outcome{cleaning.ChangeCleared, cleaning.StatusReported, cleaning.StatusCleared}),
		Entry("a stored reported finding outside the scope is omitted",
			config.CleaningActionReport, &stored{cleaning.StatusReported, true}, false, cleaning.Scope{SourceCatalog: "cat-a", TargetCatalog: "cat-c"}, nil),
	)

	It("carries a cleared finding's stored evidence, targets and edges", func() {
		row := storedOf(conflict, cleaning.StatusReported)
		row.EdgeIDs = []string{"s-j1-a1-b1", "e-j1-a1-b1"}
		plan, err := cleaning.BuildPlan(nil, []cleaning.StoredFinding{row}, planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Findings).To(HaveLen(1))
		Expect(plan.Findings[0].Finding).To(Equal(conflict))
		Expect(plan.Findings[0].EvidenceHash).To(Equal(row.EvidenceHash))
		Expect(plan.Findings[0].EdgeIDs).To(Equal([]string{"e-j1-a1-b1", "s-j1-a1-b1"}))
		Expect(plan.Retire).To(BeEmpty())
	})

	It("gives each finding the sorted, deduplicated edges of its targets only", func() {
		f := planFinding(cleaning.SignalCrossJobContradiction, keys(key("j1", a1, b1), key("j2", a1, b1), key("j3", a1, b1)), keys(key("j1", a1, b1), key("j2", a1, b1)))
		f.Evidence.Verdicts[1].EdgeIDs = []string{"s-j2-a1-b1", "e-j1-a1-b1"}
		plan, err := cleaning.BuildPlan([]cleaning.Finding{f}, nil, planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Findings[0].EdgeIDs).To(Equal([]string{"e-j1-a1-b1", "s-j1-a1-b1", "s-j2-a1-b1"}))
	})

	It("plans the targets and edges of retired findings for retirement, sorted", func() {
		low := planFinding(cleaning.SignalLowAgreement, keys(key("j2", a1, b2)), keys(key("j2", a1, b2)))
		plan, err := cleaning.BuildPlan([]cleaning.Finding{low, conflict}, nil, planPolicy(config.CleaningActionRetire), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Retire).To(Equal(keys(key("j1", a1, b1), key("j2", a1, b2))))
		Expect(plan.RetireEdgeIDs).To(Equal([]string{"e-j1-a1-b1", "e-j2-a1-b2", "s-j1-a1-b1", "s-j2-a1-b2"}))
	})

	It("retires a verdict once when two retiring findings target it", func() {
		low := planFinding(cleaning.SignalLowAgreement, keys(key("j1", a1, b1)), keys(key("j1", a1, b1)))
		plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict, low}, nil, planPolicy(config.CleaningActionRetire), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Findings).To(HaveLen(2))
		Expect(plan.Retire).To(HaveLen(1))
		Expect(plan.RetireEdgeIDs).To(Equal([]string{"e-j1-a1-b1", "s-j1-a1-b1"}))
	})

	It("does not retire for findings that keep or reach another status", func() {
		plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict}, []cleaning.StoredFinding{storedOf(conflict, cleaning.StatusRetired)},
			planPolicy(config.CleaningActionRetire), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Retire).To(BeEmpty())
		Expect(plan.RetireEdgeIDs).To(BeEmpty())
	})

	It("refuses a plan that would retire more verdicts than the cap, naming both numbers and the setting", func() {
		low := planFinding(cleaning.SignalLowAgreement, keys(key("j2", a1, b2)), keys(key("j2", a1, b2)))
		pol := planPolicy(config.CleaningActionRetire)
		pol.MaxRetirePerRun = 1
		plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict, low}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(cleaning.ErrRetireCapExceeded))
		Expect(err).To(MatchError("this run would retire 2 verdicts, more than cleaning.max_retire_per_run (1). " +
			"Narrow the run with --source-catalog/--target-catalog, or raise the setting for this tenant"))
		Expect(plan).To(Equal(cleaning.Plan{}))
	})

	It("allows a plan that retires exactly the cap", func() {
		pol := planPolicy(config.CleaningActionRetire)
		pol.MaxRetirePerRun = 1
		plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Retire).To(HaveLen(1))
	})

	It("refuses a retire action on a finding with no targets, naming the signal", func() {
		fanout := planFinding(cleaning.SignalEquivalenceFanout, keys(key("j1", a1, b1), key("j1", a1, b2)), nil)
		pol := planPolicy(config.CleaningActionRetire)
		pol.Signals.EquivalenceFanout.Action = config.CleaningActionRetire
		_, err := cleaning.BuildPlan([]cleaning.Finding{fanout}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(ContainSubstring("equivalence_fanout")))
		Expect(err).To(MatchError(ContainSubstring("report or human")))
	})

	It("refuses an action the policy does not define, naming the signal and value", func() {
		pol := planPolicy(config.CleaningActionReport)
		pol.Signals.ConflictsWith.Action = "delete"
		_, err := cleaning.BuildPlan([]cleaning.Finding{conflict}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(ContainSubstring(`cleaning.signals.conflicts_with.action "delete"`)))
	})

	It("refuses a stored finding with an unknown status, naming it", func() {
		_, err := cleaning.BuildPlan([]cleaning.Finding{conflict}, []cleaning.StoredFinding{storedOf(conflict, "archived")},
			planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(ContainSubstring(`"archived"`)))
	})

	It("refuses two detected findings with the same ID", func() {
		_, err := cleaning.BuildPlan([]cleaning.Finding{conflict, conflict}, nil, planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(ContainSubstring(cleaning.FindingID(conflict))))
	})

	It("refuses two stored findings with the same ID", func() {
		row := storedOf(conflict, cleaning.StatusReported)
		_, err := cleaning.BuildPlan(nil, []cleaning.StoredFinding{row, row}, planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
		Expect(err).To(MatchError(ContainSubstring(row.ID)))
	})

	Describe("a job-level finding", func() {
		j1Live := keys(key("j1", a1, b1), key("j1", a1, b2), key("j1", a2, b1))
		stale := jobFinding(cleaning.SignalStalePrompt, "j1", j1Live)

		DescribeTable("keeps targets and edges only when retiring",
			func(action string, wantTargets []cleaning.VerdictKey, wantEdges []string) {
				plan, err := cleaning.BuildPlan([]cleaning.Finding{stale}, nil, planPolicy(action), cleaning.Scope{}, cleaning.Summary{})
				Expect(err).NotTo(HaveOccurred())
				Expect(plan.Findings).To(HaveLen(1))
				Expect(plan.Findings[0].Targets).To(Equal(wantTargets))
				Expect(plan.Findings[0].EdgeIDs).To(Equal(wantEdges))
				Expect(plan.Retire).To(Equal(wantTargets))
				Expect(plan.RetireEdgeIDs).To(Equal(wantEdges))
			},
			Entry("report stores neither", config.CleaningActionReport, nil, nil),
			Entry("human stores neither", config.CleaningActionHuman, nil, nil),
			Entry("retire expands to the job's live verdicts", config.CleaningActionRetire, j1Live, stale.TargetEdgeIDs),
		)

		It("counts its expanded verdicts against the cap once each, beside other findings' targets", func() {
			pol := planPolicy(config.CleaningActionRetire)
			pol.MaxRetirePerRun = 3
			conflictJ1 := planFinding(cleaning.SignalConflictsWith, keys(key("j1", a2, b1)), keys(key("j1", a2, b1)))
			plan, err := cleaning.BuildPlan([]cleaning.Finding{stale, conflictJ1}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Retire).To(Equal(j1Live))

			pol.MaxRetirePerRun = 2
			_, err = cleaning.BuildPlan([]cleaning.Finding{stale, conflictJ1}, nil, pol, cleaning.Scope{}, cleaning.Summary{})
			Expect(err).To(MatchError(cleaning.ErrRetireCapExceeded))
			Expect(err).To(MatchError(ContainSubstring("this run would retire 3 verdicts, more than cleaning.max_retire_per_run (2)")))
		})

		It("is not re-raised when dismissed and only its live verdicts changed", func() {
			fewer := jobFinding(cleaning.SignalStalePrompt, "j1", j1Live[:1])
			plan, err := cleaning.BuildPlan([]cleaning.Finding{fewer}, []cleaning.StoredFinding{storedOf(stale, cleaning.StatusDismissed)},
				planPolicy(config.CleaningActionHuman), cleaning.Scope{}, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Findings).To(BeEmpty())
		})

		It("is re-raised when dismissed and a stale stage's current value changed", func() {
			moved := jobFinding(cleaning.SignalStalePrompt, "j1", j1Live)
			moved.Evidence.Detail = map[string]string{"stages": "analyzer", "analyzer.recorded": "1", "analyzer.current": "3"}
			plan, err := cleaning.BuildPlan([]cleaning.Finding{moved}, []cleaning.StoredFinding{storedOf(stale, cleaning.StatusDismissed)},
				planPolicy(config.CleaningActionHuman), cleaning.Scope{}, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Findings).To(HaveLen(1))
			Expect(plan.Findings[0].ID).To(Equal(cleaning.FindingID(stale)))
			Expect(plan.Findings[0].Change).To(Equal(cleaning.ChangeReraised))
		})

		It("refuses at plan time a finding whose stored targets would exceed their bound, naming it, the size and what a run can do", func() {
			DeferCleanup(cleaning.SetStoredListLimit(30))
			big := jobFinding(cleaning.SignalStalePrompt, "j1", j1Live[:1])
			plan, err := cleaning.BuildPlan([]cleaning.Finding{big}, nil, planPolicy(config.CleaningActionRetire), cleaning.Scope{}, cleaning.Summary{})
			Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
			Expect(err).To(MatchError(ContainSubstring("finding " + cleaning.FindingID(big) + " (stale_prompt, job j1)")))
			Expect(err).To(MatchError(ContainSubstring("the targets column would hold 51 bytes of JSON, over the 30-byte limit")))
			Expect(err).To(MatchError(ContainSubstring("Set cleaning.signals.stale_prompt.action to report or human for this tenant and apply; " +
				"a reviewer may then retire one subset that fits with 'crosscodex admin cleaning decide --retire-verdict'")))
			Expect(plan).To(Equal(cleaning.Plan{}))
		})

		It("plans the same finding when it fits the bound", func() {
			DeferCleanup(cleaning.SetStoredListLimit(51))
			_, err := cleaning.BuildPlan([]cleaning.Finding{jobFinding(cleaning.SignalStalePrompt, "j1", j1Live[:1])}, nil,
				planPolicy(config.CleaningActionRetire), cleaning.Scope{}, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	It("orders findings by ID and keeps the summary", func() {
		low := planFinding(cleaning.SignalLowAgreement, keys(key("j2", a1, b2)), keys(key("j2", a1, b2)))
		sum := cleaning.Summary{Verdicts: 2, Pairs: 2}
		plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict, low}, nil, planPolicy(config.CleaningActionReport), cleaning.Scope{}, sum)
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Findings).To(HaveLen(2))
		Expect(plan.Findings[0].ID < plan.Findings[1].ID).To(BeTrue())
		Expect(plan.Summary).To(Equal(sum))
	})

	Describe("Token", func() {
		build := func(pol config.CleaningTenantConfig, scope cleaning.Scope, sum cleaning.Summary) string {
			plan, err := cleaning.BuildPlan([]cleaning.Finding{conflict}, nil, pol, scope, sum)
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Token).To(MatchRegexp(`^[0-9a-f]{64}$`))
			return plan.Token
		}
		base := build(planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})

		It("is the same for the same inputs", func() {
			Expect(build(planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})).To(Equal(base))
		})

		It("ignores the summary", func() {
			Expect(build(planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{Verdicts: 9, SkippedEdges: 3})).To(Equal(base))
		})

		DescribeTable("changes when only the policy or scope changes",
			func(edit func(*config.CleaningTenantConfig, *cleaning.Scope)) {
				pol, scope := planPolicy(config.CleaningActionReport), cleaning.Scope{}
				edit(&pol, &scope)
				Expect(build(pol, scope, cleaning.Summary{})).NotTo(Equal(base))
			},
			Entry("another signal's action", func(p *config.CleaningTenantConfig, _ *cleaning.Scope) {
				p.Signals.StaleModel.Action = config.CleaningActionHuman
			}),
			Entry("the agreement threshold", func(p *config.CleaningTenantConfig, _ *cleaning.Scope) {
				p.Signals.LowAgreement.MinAgreementFraction = 0.75
			}),
			Entry("the retire cap", func(p *config.CleaningTenantConfig, _ *cleaning.Scope) { p.MaxRetirePerRun = 999 }),
			Entry("the scope", func(_ *config.CleaningTenantConfig, s *cleaning.Scope) {
				*s = cleaning.Scope{SourceCatalog: "cat-a", TargetCatalog: "cat-b"}
			}),
		)

		It("changes when a finding's evidence changes", func() {
			f := planFinding(cleaning.SignalConflictsWith, keys(key("j1", a1, b1)), keys(key("j1", a1, b1)))
			f.Evidence.Detail = map[string]string{"note": "x"}
			plan, err := cleaning.BuildPlan([]cleaning.Finding{f}, nil, planPolicy(config.CleaningActionReport), cleaning.Scope{}, cleaning.Summary{})
			Expect(err).NotTo(HaveOccurred())
			Expect(plan.Token).NotTo(Equal(base))
		})
	})
})

var _ = Describe("FindingID", func() {
	It("is a hex SHA-256 that differs by signal", func() {
		a := cleaning.FindingID(planFinding(cleaning.SignalConflictsWith, keys(key("j1", a1, b1)), nil))
		b := cleaning.FindingID(planFinding(cleaning.SignalLowAgreement, keys(key("j1", a1, b1)), nil))
		Expect(a).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(a).NotTo(Equal(b))
	})

	It("ignores evidence and targets", func() {
		f := planFinding(cleaning.SignalConflictsWith, keys(key("j1", a1, b1)), keys(key("j1", a1, b1)))
		g := f
		g.Targets, g.Evidence = nil, cleaning.Evidence{}
		Expect(cleaning.FindingID(g)).To(Equal(cleaning.FindingID(f)))
	})

	It("keys a job-level finding by its signal and job alone", func() {
		base := jobFinding(cleaning.SignalStalePrompt, "j1", keys(key("j1", a1, b1), key("j1", a1, b2)))
		other := jobFinding(cleaning.SignalStalePrompt, "j1", keys(key("j1", a2, b1)))
		other.Evidence.Detail = map[string]string{"stages": "synthesized"}
		other.SourceCatalog, other.TargetCatalog = "cat-x", "cat-y"
		Expect(cleaning.FindingID(other)).To(Equal(cleaning.FindingID(base)))
		Expect(cleaning.FindingID(jobFinding(cleaning.SignalStalePrompt, "j2", nil))).NotTo(Equal(cleaning.FindingID(base)))
		Expect(cleaning.FindingID(jobFinding(cleaning.SignalStaleModel, "j1", nil))).NotTo(Equal(cleaning.FindingID(base)))
	})

	It("never gives a job-keyed and a verdict-keyed finding the same ID", func() {
		job := cleaning.FindingID(jobFinding(cleaning.SignalStalePrompt, "j1", nil))
		for _, concerns := range [][]cleaning.VerdictKey{
			nil,
			keys(key("j1", a1, b1)),
			keys(cleaning.VerdictKey{JobID: "job", SourceID: "j1", TargetID: "x"}),
		} {
			Expect(cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalStalePrompt, Concerns: concerns})).NotTo(Equal(job))
		}
	})

	It("cannot be forged by moving a newline between concern keys", func() {
		x := cleaning.Finding{Signal: cleaning.SignalStalePrompt, Concerns: keys(
			cleaning.VerdictKey{JobID: "j", SourceID: "s", TargetID: "t\nA"}, cleaning.VerdictKey{JobID: "B", SourceID: "c", TargetID: "d"})}
		y := cleaning.Finding{Signal: cleaning.SignalStalePrompt, Concerns: keys(
			cleaning.VerdictKey{JobID: "j", SourceID: "s", TargetID: "t"}, cleaning.VerdictKey{JobID: "A\nB", SourceID: "c", TargetID: "d"})}
		Expect(cleaning.FindingID(x)).NotTo(Equal(cleaning.FindingID(y)))
	})
})

var _ = Describe("EvidenceHash", func() {
	ev := planFinding(cleaning.SignalConflictsWith, keys(key("j1", a1, b1)), nil).Evidence

	It("is a hex SHA-256 that changes with the evidence", func() {
		h, err := cleaning.EvidenceHash(ev)
		Expect(err).NotTo(HaveOccurred())
		Expect(h).To(MatchRegexp(`^[0-9a-f]{64}$`))
		other := ev
		other.Detail = map[string]string{"latest_job": "j2"}
		h2, err := cleaning.EvidenceHash(other)
		Expect(err).NotTo(HaveOccurred())
		Expect(h2).NotTo(Equal(h))
	})

	It("is the same for one instant in different time zones", func() {
		h, err := cleaning.EvidenceHash(ev)
		Expect(err).NotTo(HaveOccurred())
		local := cleaning.Evidence{Verdicts: []cleaning.VerdictEvidence{ev.Verdicts[0]}}
		local.Verdicts[0].JobCompletedAt = t1.In(time.FixedZone("UTC+5", 5*3600))
		h2, err := cleaning.EvidenceHash(local)
		Expect(err).NotTo(HaveOccurred())
		Expect(h2).To(Equal(h))
		Expect(ev.Verdicts[0].JobCompletedAt.Location()).To(Equal(time.UTC))
	})

	It("is the same after the evidence round-trips through storage", func() {
		h, err := cleaning.EvidenceHash(ev)
		Expect(err).NotTo(HaveOccurred())
		b, err := json.Marshal(ev)
		Expect(err).NotTo(HaveOccurred())
		parsed, err := cleaning.ParseEvidence(b)
		Expect(err).NotTo(HaveOccurred())
		h2, err := cleaning.EvidenceHash(parsed)
		Expect(err).NotTo(HaveOccurred())
		Expect(h2).To(Equal(h))
	})

	It("ignores the verdicts' edge IDs and leaves the caller's evidence unchanged", func() {
		h, err := cleaning.EvidenceHash(ev)
		Expect(err).NotTo(HaveOccurred())
		restored := cleaning.Evidence{Verdicts: []cleaning.VerdictEvidence{ev.Verdicts[0]}}
		restored.Verdicts[0].EdgeIDs = []string{"restored-1", "restored-2"}
		h2, err := cleaning.EvidenceHash(restored)
		Expect(err).NotTo(HaveOccurred())
		Expect(h2).To(Equal(h))
		Expect(restored.Verdicts[0].EdgeIDs).To(Equal([]string{"restored-1", "restored-2"}))
	})

	Describe("of a job-level finding", func() {
		job := jobFinding(cleaning.SignalStalePrompt, "j1", keys(key("j1", a1, b1))).Evidence
		hashOf := func(e cleaning.Evidence) string {
			h, err := cleaning.EvidenceHash(e)
			Expect(err).NotTo(HaveOccurred())
			return h
		}
		base := hashOf(job)
		edited := func(edit func(*cleaning.JobEvidence, map[string]string)) cleaning.Evidence {
			j := *job.Job
			d := map[string]string{}
			for k, v := range job.Detail {
				d[k] = v
			}
			edit(&j, d)
			return cleaning.Evidence{Job: &j, Detail: d}
		}

		DescribeTable("ignores what does not make the job stale",
			func(edit func(*cleaning.JobEvidence, map[string]string)) {
				Expect(hashOf(edited(edit))).To(Equal(base))
			},
			Entry("the count of verdicts in scope", func(j *cleaning.JobEvidence, _ map[string]string) { j.VerdictsInScope = 2000 }),
			Entry("the catalog display names", func(j *cleaning.JobEvidence, _ map[string]string) {
				j.SourceCatalog, j.TargetCatalog = "cat-a", "SP 800-171 r3"
			}),
			Entry("the completion time's zone", func(j *cleaning.JobEvidence, _ map[string]string) {
				j.CompletedAt = j.CompletedAt.In(time.FixedZone("UTC+5", 5*3600))
			}),
		)

		DescribeTable("changes with what makes the job stale",
			func(edit func(*cleaning.JobEvidence, map[string]string)) {
				Expect(hashOf(edited(edit))).NotTo(Equal(base))
			},
			Entry("the job", func(j *cleaning.JobEvidence, _ map[string]string) { j.ID = "j2" }),
			Entry("the job's completion time", func(j *cleaning.JobEvidence, _ map[string]string) { j.CompletedAt = t2 }),
			Entry("a recorded stage value", func(_ *cleaning.JobEvidence, d map[string]string) { d["analyzer.recorded"] = "0" }),
			Entry("a current stage value", func(_ *cleaning.JobEvidence, d map[string]string) { d["analyzer.current"] = "3" }),
			Entry("the stale stages", func(_ *cleaning.JobEvidence, d map[string]string) { d["stages"] = "analyzer,synthesized" }),
		)

		It("leaves the caller's evidence unchanged", func() {
			Expect(job.Job.VerdictsInScope).To(Equal(1))
			Expect(job.Job.SourceCatalog).To(Equal("SP 800-53 5.1.1"))
		})
	})
})

var _ = Describe("StoredFinding.Validate", func() {
	stale := jobFinding(cleaning.SignalStalePrompt, "j1", keys(key("j1", a1, b1)))

	It("accepts a job-level finding with its targets and edges", func() {
		row := storedOf(stale, cleaning.StatusRetired)
		row.EdgeIDs = stale.TargetEdgeIDs
		Expect(row.Validate()).To(Succeed())
	})

	DescribeTable("refuses a finding whose subject does not match its signal, saying nothing was written",
		func(row cleaning.StoredFinding, want string) {
			err := row.Validate()
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(MatchError(ContainSubstring("nothing was written")))
		},
		Entry("a stale signal keyed by verdicts",
			storedOf(planFinding(cleaning.SignalStalePrompt, keys(key("j1", a1, b1)), nil), cleaning.StatusReported),
			"signal stale_prompt is raised per job, so its evidence must name the job"),
		Entry("a per-verdict signal naming a job",
			storedOf(jobFinding(cleaning.SignalLowAgreement, "j1", nil), cleaning.StatusReported),
			"signal low_agreement is raised per verdict, so its evidence must not name a job"),
		Entry("a job-level finding that also concerns verdicts",
			func() cleaning.StoredFinding {
				r := storedOf(stale, cleaning.StatusReported)
				r.Concerns = keys(key("j1", a1, b1))
				return r
			}(),
			"a job-level finding concerns its job, not verdicts"),
		Entry("a job ID holding the key separator",
			storedOf(jobFinding(cleaning.SignalStalePrompt, "j:1", nil), cleaning.StatusReported),
			"job part contains the separator"),
	)

	It("refuses edge IDs over their bound with ErrFindingTooLarge, saying nothing was written", func() {
		DeferCleanup(cleaning.SetStoredListLimit(60))
		row := storedOf(stale, cleaning.StatusRetired)
		row.EdgeIDs = []string{strings.Repeat("e", 60)}
		err := row.Validate()
		Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
		Expect(err).To(MatchError(ContainSubstring("the edge_ids column would hold 64 bytes of JSON, over the 60-byte limit")))
		Expect(err).To(MatchError(ContainSubstring("nothing was written")))
		Expect(err).To(MatchError(ContainSubstring("A cleaning run refuses this at preview and a decision before it is signed")))
	})
})

var _ = Describe("StateToken", func() {
	row := cleaning.StoredFinding{ID: "f1", Status: cleaning.StatusPending, EvidenceHash: "h1", EdgeIDs: []string{"e1", "e2"}}
	base := cleaning.StateToken(row)

	It("is a hex SHA-256 that ignores edge ID order", func() {
		Expect(base).To(MatchRegexp(`^[0-9a-f]{64}$`))
		r := row
		r.EdgeIDs = []string{"e2", "e1"}
		Expect(cleaning.StateToken(r)).To(Equal(base))
		Expect(row.EdgeIDs).To(Equal([]string{"e1", "e2"}))
	})

	DescribeTable("changes when the finding changes",
		func(edit func(*cleaning.StoredFinding)) {
			r := row
			edit(&r)
			Expect(cleaning.StateToken(r)).NotTo(Equal(base))
		},
		Entry("status", func(r *cleaning.StoredFinding) { r.Status = cleaning.StatusRetired }),
		Entry("evidence hash", func(r *cleaning.StoredFinding) { r.EvidenceHash = "h2" }),
		Entry("edge IDs", func(r *cleaning.StoredFinding) { r.EdgeIDs = []string{"e1", "e3"} }),
		Entry("an edge ID split differently", func(r *cleaning.StoredFinding) { r.EdgeIDs = []string{"e1e2"} }),
	)
})
