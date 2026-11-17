package cleaning

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

// signalOrder is the order Detect returns findings in.
var signalOrder = []Signal{
	SignalEquivalenceFanout, SignalCrossJobContradiction, SignalLowAgreement,
	SignalConflictsWith, SignalStalePrompt, SignalStaleModel,
}

// Signals returns every signal, in the order Detect returns findings in.
func Signals() []Signal { return slices.Clone(signalOrder) }

// Valid reports whether s is one of Signals.
func (s Signal) Valid() bool { return slices.Contains(signalOrder, s) }

// Finding is one raised signal: the verdicts it concerns, the subset that
// retiring would remove, and what a reviewer needs to judge it. A job-level
// finding (Signal.JobLevel) concerns its job, named in Evidence.Job, instead
// of verdicts.
type Finding struct {
	Signal Signal
	// Concerns are every verdict the finding is about, sorted; nil for a
	// job-level finding.
	Concerns []VerdictKey
	// Targets are the verdicts a retire action removes, sorted; nil when the
	// data cannot say which verdict is wrong. For a job-level finding they
	// are the job's live verdicts the run read, which BuildPlan keeps only
	// when the finding is retired.
	Targets []VerdictKey
	// TargetEdgeIDs are, for a job-level finding, the sorted live edges of
	// Targets, which its evidence does not list; nil otherwise.
	TargetEdgeIDs []string
	Evidence      Evidence
	// SourceCatalog and TargetCatalog are the catalog IDs (not display
	// labels) that decide whether a scoped run covers the finding: the first
	// concern's stored source and target controls', or a job-level finding's
	// job's.
	SourceCatalog, TargetCatalog string
}

// Summary counts what a detection run read.
type Summary struct {
	Verdicts, Pairs int
	// UnknownPrompt and UnknownModel count jobs whose staleness for that
	// signal could not be decided; see Detect.
	UnknownPrompt, UnknownModel int
	// UnaddressableVerdicts counts verdicts excluded from detection because
	// their key fails ParseVerdictKey's checks (an ID part that is empty,
	// holds ':' or exceeds 256 bytes), so no stored finding could name them.
	// Verdicts and Pairs count only what detection examined.
	UnaddressableVerdicts int
	// SkippedEdges is filled by the caller from crosswalk.LoadPair; Detect
	// leaves it zero.
	SkippedEdges int64
}

// Current maps a stage name to the record that stage would write if it ran
// now.
type Current map[string]StageRecord

// Detect runs every signal over pairs and returns the findings, ordered by
// signal (equivalence_fanout, cross_job_contradiction, low_agreement,
// conflicts_with, stale_prompt, stale_model) and then by Concerns. Verdicts
// with an unaddressable key are excluded first (see Summary). Signals run
// whatever action pol configures; only MinAgreementFraction is read from it.
//
//   - equivalence_fanout: a control whose merged verdict is direct EQUIVALENT
//     with two or more controls of one other catalog. Concerns are the latest
//     job's verdicts on those pairs; no targets.
//   - cross_job_contradiction: verdicts of two or more distinct jobs on a pair
//     disagree once reversed verdicts are inverted. Concerns are all of the
//     pair's verdicts; targets are those not from crosswalk.Merge's LatestJob.
//     A job's own verdicts (both directions) never contradict each other.
//   - low_agreement: a verdict with Agreement below MinAgreementFraction.
//   - conflicts_with: a verdict of type CONFLICTS_WITH.
//   - stale_prompt and stale_model: at most one job-level finding per job and
//     signal, whose evidence names the job and counts its verdicts in pairs,
//     and whose targets are those verdicts; a job without verdicts in pairs
//     raises none. Only stages present in current are compared,
//     with PromptStale and ModelStale. Evidence detail "stages" lists every
//     stale stage, sorted and comma-separated, and "<stage>.recorded" and
//     "<stage>.current" hold the compared values: the prompt version, or the
//     models as sorted "alias=revision" joined by ",".
//
// A compared stage whose record is nil or missing is unknown, never stale;
// for stale_prompt so is one whose recorded or current prompt version is "".
// A job adds one to UnknownPrompt (UnknownModel) when it has at least one
// unknown compared stage and no stage stale for that signal.
func Detect(pairs []Pair, jobs map[string]Job, current Current, pol config.CleaningTenantConfig) ([]Finding, Summary) {
	d := detector{byKey: map[VerdictKey]Verdict{}, byJob: map[string][]VerdictKey{}, jobs: jobs}
	var sum Summary
	addressable := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		kept := Pair{A: p.A, B: p.B}
		for _, v := range p.Verdicts {
			if v.Key.validate() != nil {
				sum.UnaddressableVerdicts++
				continue
			}
			kept.Verdicts = append(kept.Verdicts, v)
			d.byKey[v.Key] = v
			d.byJob[v.Key.JobID] = append(d.byJob[v.Key.JobID], v.Key)
		}
		if len(kept.Verdicts) > 0 {
			addressable = append(addressable, kept)
			sum.Verdicts += len(kept.Verdicts)
		}
	}
	pairs = addressable
	sum.Pairs = len(pairs)

	findings := d.equivalenceFanout(pairs)
	findings = append(findings, d.contradictions(pairs)...)
	findings = append(findings, d.perVerdict(pairs, pol.Signals.LowAgreement.MinAgreementFraction)...)
	findings = append(findings, d.staleness(current, &sum)...)

	slices.SortStableFunc(findings, func(x, y Finding) int {
		return cmp.Or(
			cmp.Compare(slices.Index(signalOrder, x.Signal), slices.Index(signalOrder, y.Signal)),
			cmp.Compare(jobOf(x), jobOf(y)),
			slices.CompareFunc(x.Concerns, y.Concerns, compareKeys),
		)
	})
	return findings, sum
}

// jobOf returns a job-level finding's job ID, else "".
func jobOf(f Finding) string {
	if f.Evidence.Job == nil {
		return ""
	}
	return f.Evidence.Job.ID
}

// detector indexes the verdicts so findings can carry readable evidence.
type detector struct {
	byKey map[VerdictKey]Verdict
	byJob map[string][]VerdictKey
	jobs  map[string]Job
}

func (d detector) finding(s Signal, concerns, targets []VerdictKey, detail map[string]string) Finding {
	f := Finding{Signal: s, Concerns: sortedKeys(concerns), Targets: sortedKeys(targets)}
	f.Evidence = Evidence{Verdicts: make([]VerdictEvidence, len(f.Concerns)), Detail: detail}
	for i, k := range f.Concerns {
		f.Evidence.Verdicts[i] = evidenceOf(d.byKey[k], d.jobs[k.JobID])
	}
	first := d.byKey[f.Concerns[0]] // every signal concerns at least one verdict
	f.SourceCatalog, f.TargetCatalog = nodeString(first.Source, "catalog_id"), nodeString(first.Target, "catalog_id")
	return f
}

func sortedKeys(ks []VerdictKey) []VerdictKey {
	if len(ks) == 0 {
		return nil
	}
	out := slices.Clone(ks)
	slices.SortFunc(out, compareKeys)
	return out
}

func merge(vs []Verdict) crosswalk.Merged {
	cv := make([]crosswalk.Verdict, len(vs))
	for i, v := range vs {
		cv[i] = crosswalk.Verdict{JobID: v.Key.JobID, CompletedAt: v.CompletedAt, Type: v.Type, Confidence: v.Confidence, Reversed: v.Reversed}
	}
	return crosswalk.Merge(cv)
}

func (d detector) equivalenceFanout(pairs []Pair) []Finding {
	var edges []crosswalk.EquivalenceEdge
	var concernsOf [][]VerdictKey // edges index -> the latest job's verdicts on that pair
	for _, p := range pairs {
		m := merge(p.Verdicts)
		if m.Type != pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT || m.Direction != crosswalk.Direct {
			continue
		}
		edges = append(edges, crosswalk.EquivalenceEdge{
			SourceNode: p.A.ID, SourceCatalog: nodeString(p.A, "catalog_id"),
			TargetNode: p.B.ID, TargetCatalog: nodeString(p.B, "catalog_id"),
		})
		var latest []VerdictKey
		for _, v := range p.Verdicts {
			if v.Key.JobID == m.LatestJob {
				latest = append(latest, v.Key)
			}
		}
		concernsOf = append(concernsOf, latest)
	}
	var out []Finding
	for _, g := range crosswalk.EquivalenceGroups(edges) {
		var concerns []VerdictKey
		for _, m := range g.Members {
			concerns = append(concerns, concernsOf[m]...)
		}
		out = append(out, d.finding(SignalEquivalenceFanout, concerns, nil, nil))
	}
	return out
}

// reading is a verdict's type in the pair's A-to-B direction. A reversed
// verdict whose type cannot be inverted keeps reversed set, so it never
// equals a forward reading.
type reading struct {
	typ      pb.RelationshipType
	reversed bool
}

func readingOf(v Verdict) reading {
	if v.Reversed {
		if inv, ok := crosswalk.Invert(v.Type); ok {
			return reading{inv, false}
		}
	}
	return reading{v.Type, v.Reversed}
}

func (d detector) contradictions(pairs []Pair) []Finding {
	var out []Finding
	for _, p := range pairs {
		if !crossJobDisagreement(p.Verdicts) {
			continue
		}
		latest := merge(p.Verdicts).LatestJob
		concerns := make([]VerdictKey, len(p.Verdicts))
		var targets []VerdictKey
		for i, v := range p.Verdicts {
			concerns[i] = v.Key
			if v.Key.JobID != latest {
				targets = append(targets, v.Key)
			}
		}
		out = append(out, d.finding(SignalCrossJobContradiction, concerns, targets, map[string]string{"latest_job": latest}))
	}
	return out
}

// crossJobDisagreement reports whether two verdicts of different jobs read
// the pair differently.
func crossJobDisagreement(vs []Verdict) bool {
	for i, v := range vs {
		for _, w := range vs[i+1:] {
			if v.Key.JobID != w.Key.JobID && readingOf(v) != readingOf(w) {
				return true
			}
		}
	}
	return false
}

// perVerdict raises the signals that judge one verdict alone: low_agreement
// and conflicts_with.
func (d detector) perVerdict(pairs []Pair, minAgreement float64) []Finding {
	lowDetail := map[string]string{"min_agreement_fraction": strconv.FormatFloat(minAgreement, 'f', -1, 64)}
	var out []Finding
	for _, p := range pairs {
		for _, v := range p.Verdicts {
			self := []VerdictKey{v.Key}
			if v.Agreement != nil && *v.Agreement < minAgreement {
				out = append(out, d.finding(SignalLowAgreement, self, self, maps.Clone(lowDetail)))
			}
			if v.Type == pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH {
				out = append(out, d.finding(SignalConflictsWith, self, self, nil))
			}
		}
	}
	return out
}

// staleCheck is one staleness signal: how to compare a stage and how to show
// the compared values.
type staleCheck struct {
	signal  Signal
	stale   func(recorded *StageRecord, current StageRecord) bool
	known   func(recorded *StageRecord, current StageRecord) bool
	render  func(StageRecord) string
	unknown *int
}

func (d detector) staleness(current Current, sum *Summary) []Finding {
	stages := slices.Sorted(maps.Keys(current))
	checks := []staleCheck{
		{SignalStalePrompt, PromptStale, promptKnown, func(r StageRecord) string { return r.PromptVersion }, &sum.UnknownPrompt},
		{SignalStaleModel, ModelStale, modelKnown, renderModels, &sum.UnknownModel},
	}
	var out []Finding
	for _, id := range slices.Sorted(maps.Keys(d.jobs)) {
		job := d.jobs[id]
		for _, c := range checks {
			var stale []string
			detail := map[string]string{}
			unknown := false
			for _, stage := range stages {
				rec := job.Stages[stage]
				if !c.known(rec, current[stage]) {
					unknown = true
					continue
				}
				if c.stale(rec, current[stage]) {
					stale = append(stale, stage)
					detail[stage+".recorded"] = c.render(*rec)
					detail[stage+".current"] = c.render(current[stage])
				}
			}
			switch {
			case len(stale) > 0 && len(d.byJob[id]) > 0:
				detail["stages"] = strings.Join(stale, ",")
				out = append(out, d.jobFinding(c.signal, job, detail))
			case len(stale) == 0 && unknown:
				*c.unknown++
			}
		}
	}
	return out
}

// jobFinding is signal s raised on job: its evidence names the job and counts
// its verdicts in the pairs read, and its targets are those verdicts.
func (d detector) jobFinding(s Signal, job Job, detail map[string]string) Finding {
	targets := sortedKeys(d.byJob[job.ID])
	var edges []string
	for _, k := range targets {
		edges = append(edges, d.byKey[k].EdgeIDs...)
	}
	return Finding{
		Signal: s, Targets: targets, TargetEdgeIDs: sortedUnique(edges),
		Evidence: Evidence{Job: &JobEvidence{
			ID: job.ID, CompletedAt: job.CompletedAt,
			SourceCatalog:   catalogDisplay(job.Catalogs, job.Catalogs.SourceID),
			TargetCatalog:   catalogDisplay(job.Catalogs, job.Catalogs.TargetID),
			VerdictsInScope: len(targets),
		}, Detail: detail},
		SourceCatalog: job.Catalogs.SourceID, TargetCatalog: job.Catalogs.TargetID,
	}
}

// renderModels shows a panel as sorted "alias=revision" pairs joined by ",".
func renderModels(r StageRecord) string {
	parts := make([]string, 0, len(r.Models))
	for _, alias := range slices.Sorted(maps.Keys(r.Models)) {
		parts = append(parts, alias+"="+r.Models[alias])
	}
	return strings.Join(parts, ",")
}
