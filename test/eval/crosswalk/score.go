package crosswalk

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// unattributed is the generator a candidate without contributions counts under.
const unattributed = "unattributed"

// ScoreInput is everything one score reads. Source, Subset and Target are
// read for their digests; Target also defines base controls. Baseline is
// optional.
type ScoreInput struct {
	Profile                *Profile
	Source, Subset, Target *Catalog
	Refs                   References
	Run                    *Run
	Baseline               *Report
	// CSVDigest is CSVDigest(Profile, root); it must be set when the profile
	// names a references.csv.
	CSVDigest     string
	Label, Commit string
	// Labels are the loaded label file's judgements keyed by the pair as the
	// run exported it; nil or empty when the profile has none.
	Labels map[Pair]Label
	// LabelsDigest is the digest LoadLabels returned with Labels; empty
	// without labels.
	LabelsDigest string
}

func (p Pair) String() string { return p.SourceID + " -> " + p.TargetID }

func comparePairs(a, b Pair) int {
	return cmp.Or(cmp.Compare(a.SourceID, b.SourceID), cmp.Compare(a.TargetID, b.TargetID))
}

type pairSet map[Pair]bool

// Score scores in.Run against the profile's resolved references. It fails
// when the profile resolved no references, when the run names a control
// outside the profile's controls or the target catalog (the wrong job or
// catalog), when a related mapping is not a candidate (found ⊆ reachable must
// hold), and when the baseline was scored with another profile.
func Score(in ScoreInput) (*Report, error) {
	p := in.Profile
	if len(in.Refs.Resolved) == 0 {
		return nil, fmt.Errorf("profile %s extracted no references from %s; run `task eval:crosswalk:discover SOURCE=%s TARGET=%s` to see which cited resources resolve",
			p.Name, p.Source, p.Source, p.Target)
	}
	if p.References.CSV != "" && in.CSVDigest == "" {
		return nil, fmt.Errorf("profile %s names references.csv %q but no digest of it was given: hash the file with CSVDigest so the report names the exact yardstick", p.Name, p.References.CSV)
	}
	sourceCatalogID, targetCatalogID, err := analysisCatalogs(in.Run)
	if err != nil {
		return nil, err
	}

	idx := NewTargetIndex(in.Target)
	srcIdx := NewTargetIndex(in.Subset)
	inScope := map[string]bool{}
	for _, c := range p.Controls {
		inScope[c] = true
	}
	// check validates a pair as exported and returns it rolled up to controls
	// on both sides: the analyzers map the importer's statement items, and an
	// item counts for the control that contains it (the profile control on the
	// source side, the owning target control on the target side).
	check := func(what string, pr Pair) (Pair, error) {
		owner := srcIdx.Owner(pr.SourceID)
		if !inScope[owner] {
			return Pair{}, fmt.Errorf("%s %s: source %q is neither one of profile %s's controls nor a statement item of one: score the job that ran on this profile's subset",
				what, pr, pr.SourceID, p.Name)
		}
		if !idx.Has(pr.TargetID) {
			return Pair{}, fmt.Errorf("%s %s: target %q is not a control of target catalog %q: score against the catalog the job mapped onto",
				what, pr, pr.TargetID, in.Target.Path)
		}
		return Pair{SourceID: owner, TargetID: idx.Owner(pr.TargetID)}, nil
	}
	base := func(pr Pair) Pair { return Pair{SourceID: pr.SourceID, TargetID: idx.Base(pr.TargetID)} }

	for _, r := range in.Refs.Resolved {
		if _, err := check("reference", Pair{r.SourceID, r.TargetID}); err != nil {
			return nil, err
		}
	}

	// reachable and found hold pairs rolled up to the profile control;
	// candidate holds the pairs as exported, for the found ⊆ candidates check.
	reachable, reachableBase, candidate := pairSet{}, pairSet{}, pairSet{}
	byGenerator := map[string][2]pairSet{}
	for _, c := range in.Run.Candidates {
		raw := Pair{c.SourceID, c.TargetID}
		pr, err := check("candidate", raw)
		if err != nil {
			return nil, err
		}
		candidate[raw] = true
		reachable[pr], reachableBase[base(pr)] = true, true
		gens := c.Generators
		if len(gens) == 0 {
			gens = []string{unattributed}
		}
		for _, g := range gens {
			sets, ok := byGenerator[g]
			if !ok {
				sets = [2]pairSet{{}, {}}
				byGenerator[g] = sets
			}
			sets[0][pr], sets[1][base(pr)] = true, true
		}
	}

	seen := map[Pair]int{}
	for _, m := range in.Run.Mappings {
		seen[Pair{m.SourceID, m.TargetID}]++
	}
	found, foundBase := pairSet{}, pairSet{}
	agrees := pairSet{}
	refExact, refBase := pairSet{}, pairSet{}
	for _, r := range in.Refs.Resolved {
		refExact[Pair{r.SourceID, r.TargetID}] = true
		refBase[base(Pair{r.SourceID, r.TargetID})] = true
	}
	for _, m := range in.Run.Mappings {
		raw := Pair{m.SourceID, m.TargetID}
		pr, err := check("mapping", raw)
		if err != nil {
			return nil, err
		}
		if n := seen[raw]; n > 1 {
			return nil, fmt.Errorf("related mapping %s appears %d times in the results export of job %s: export one stage (--stage analyzer) of one job",
				raw, n, in.Run.JobID)
		}
		if !candidate[raw] {
			return nil, fmt.Errorf("related mapping %s is not a candidate of job %s, but the relationship analyzer judges only candidates: "+
				"export the mappings and candidates of the same job, or report a pipeline bug", raw, in.Run.JobID)
		}
		found[pr], foundBase[base(pr)] = true, true
		// A row agrees with the reference exactly as recall credits it:
		// the same pair, or the same source and base control.
		agrees[raw] = refExact[pr] || refBase[base(pr)]
	}

	rep := &Report{
		Provenance: Provenance{
			Label: in.Label, Commit: in.Commit, JobID: in.Run.JobID,
			SourceCatalogID: sourceCatalogID, TargetCatalogID: targetCatalogID,
			Profile:      ProfileIdentity{Name: p.Name, Digest: p.Digest, Controls: slices.Clone(p.Controls), CSVDigest: in.CSVDigest},
			SourceDigest: in.Source.Digest, SubsetDigest: in.Subset.Digest, TargetDigest: in.Target.Digest, LabelsDigest: in.LabelsDigest,
		},
		Cost: stageCosts(in.Run.Stages),
	}
	rep.Cost.Analyzers = slices.Clone(in.Run.Costs)
	rep.Cost.RelationshipReuse = relationshipReuse(in.Run.Costs)
	for _, u := range distinctRaw(in.Refs.Unresolved) {
		rep.Unresolved = append(rep.Unresolved, UnresolvedReference{SourceID: u.SourceID, Identifier: u.Identifier, Citation: u.Citation})
	}
	for _, u := range distinctRaw(in.Refs.Ambiguous) {
		rep.Ambiguous = append(rep.Ambiguous, UnresolvedReference{
			SourceID: u.SourceID, Identifier: u.Identifier, Citation: u.Citation, Candidates: idx.Resolve(u.Identifier).Candidates,
		})
	}
	resolved := len(distinctRaw(in.Refs.ResolvedRaw))
	rep.Resolution = newRate(resolved, resolved+len(rep.Unresolved)+len(rep.Ambiguous))

	var fe, fb, re, rb int
	genHits := map[string][2]int{}
	for _, r := range in.Refs.Resolved {
		pr := Pair{r.SourceID, r.TargetID}
		sr := ScoredReference{
			SourceID: r.SourceID, TargetID: r.TargetID, Citation: r.Citation,
			FoundExact: found[pr], FoundBase: foundBase[base(pr)],
			ReachableExact: reachable[pr], ReachableBase: reachableBase[base(pr)],
		}
		if sr.FoundExact {
			fe++
		}
		if sr.FoundBase {
			fb++
		}
		if sr.ReachableExact {
			re++
		}
		if sr.ReachableBase {
			rb++
		}
		for g, sets := range byGenerator {
			h := genHits[g]
			if sets[0][pr] {
				h[0]++
			}
			if sets[1][base(pr)] {
				h[1]++
			}
			genHits[g] = h
		}
		rep.References = append(rep.References, sr)
	}
	total := len(in.Refs.Resolved)
	rep.Recall = Recall{
		FoundExact: newRate(fe, total), FoundBase: newRate(fb, total),
		ReachableExact: newRate(re, total), ReachableBase: newRate(rb, total),
	}
	if len(genHits) > 0 {
		rep.Recall.ByGenerator = map[string]Reachability{}
		for g, h := range genHits {
			rep.Recall.ByGenerator[g] = Reachability{Exact: newRate(h[0], total), Base: newRate(h[1], total)}
		}
	}

	rep.Related = slices.Clone(in.Run.Mappings)
	slices.SortFunc(rep.Related, func(a, b Mapping) int {
		return comparePairs(Pair{a.SourceID, a.TargetID}, Pair{b.SourceID, b.TargetID})
	})
	rep.Distribution = distribution(rep.Related)
	rep.Precision = precision(rep.Related, agrees, in.Labels)

	if in.Baseline != nil {
		if err := sameYardstick(in.Baseline, rep); err != nil {
			return nil, err
		}
		rel, _ := relationshipCost(rep.Cost.Analyzers)
		rep.Agreement = agreement(in.Baseline, rep.Related, rel)
	}
	return rep, nil
}

// precision scores the produced rows against the reference (agrees, keyed by
// the pair as exported) and against labels. Rows are sorted, so Unlabelled and
// Disputed are too. Without labels only the reference rate is set.
func precision(rows []Mapping, agrees pairSet, labels map[Pair]Label) Precision {
	var p Precision
	agreeing, related, judged := 0, 0, 0
	for _, m := range rows {
		pr := Pair{m.SourceID, m.TargetID}
		if agrees[pr] {
			agreeing++
		}
		if len(labels) == 0 {
			continue
		}
		switch l, ok := labels[pr]; {
		case !ok:
			p.Unlabelled = append(p.Unlabelled, pr)
		case l.Verdict == VerdictDisputed:
			p.Disputed = append(p.Disputed, pr)
		default:
			judged++
			if l.Verdict == VerdictRelated {
				related++
			}
		}
	}
	p.Reference = newRate(agreeing, len(rows))
	if len(labels) > 0 {
		lr, cov := newRate(related, judged), newRate(judged, len(rows))
		p.Labelled, p.Coverage = &lr, &cov
	}
	return p
}

// distinctRaw returns refs without repeats of a (source, identifier), keeping
// the first citation, in input order.
func distinctRaw(refs []RawReference) []RawReference {
	seen := map[[2]string]bool{}
	var out []RawReference
	for _, r := range refs {
		if k := [2]string{r.SourceID, r.Identifier}; !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// relationshipCost returns the cost of the analyzer named
// results.RelationshipAnalyzer (the pipeline's stage name, exactly).
func relationshipCost(costs []AnalyzerCost) (AnalyzerCost, bool) {
	for _, c := range costs {
		if c.Analyzer == results.RelationshipAnalyzer {
			return c, true
		}
	}
	return AnalyzerCost{}, false
}

// relationshipReuse returns the hits and tasks of the relationship analyzer, or
// nil when the job has none.
func relationshipReuse(costs []AnalyzerCost) *Reuse {
	if c, ok := relationshipCost(costs); ok {
		return &Reuse{Hits: c.Hits, Tasks: c.Tasks}
	}
	return nil
}

// analysisCatalogs returns the source and target catalog IDs the results
// export's analysis names, refusing a bidirectional job.
func analysisCatalogs(run *Run) (string, string, error) {
	var a struct {
		Bidirectional bool `json:"bidirectional"`
		Source        struct {
			ID string `json:"catalogId"`
		} `json:"sourceCatalog"`
		Target struct {
			ID string `json:"catalogId"`
		} `json:"targetCatalog"`
	}
	if len(run.Analysis) > 0 {
		if err := json.Unmarshal(run.Analysis, &a); err != nil {
			return "", "", fmt.Errorf("decoding the analysis of job %s: %w", run.JobID, err)
		}
	}
	if a.Bidirectional {
		return "", "", fmt.Errorf("job %s is bidirectional; the eval scores one direction (source → target). Start the job without --bidirectional.", run.JobID) //nolint:staticcheck // ST1005: the exact sentence is the specified, user-facing message
	}
	if a.Source.ID == "" || a.Target.ID == "" {
		return "", "", fmt.Errorf("the results export of job %s has no analysis naming its source and target catalogs, so the report could not name them: "+
			"export with a crosscodex version whose results export prints \"analysis\"", run.JobID)
	}
	return a.Source.ID, a.Target.ID, nil
}

func stageCosts(stages []Stage) Cost {
	var c Cost
	var first, last time.Time
	complete := len(stages) > 0
	for _, s := range stages {
		sc := StageCost{Name: s.Name, Status: s.Status, StartedAt: s.Started, CompletedAt: s.Completed}
		if s.Started.IsZero() || s.Completed.IsZero() {
			complete = false
		} else {
			secs := s.Completed.Sub(s.Started).Seconds()
			sc.Seconds = &secs
			if first.IsZero() || s.Started.Before(first) {
				first = s.Started
			}
			if s.Completed.After(last) {
				last = s.Completed
			}
		}
		c.Stages = append(c.Stages, sc)
	}
	if complete {
		secs := last.Sub(first).Seconds()
		c.TotalSeconds = &secs
	}
	return c
}

func distribution(ms []Mapping) Distribution {
	var d Distribution
	confidences := make([]float64, 0, len(ms))
	for _, m := range ms {
		if d.Types == nil {
			d.Types = map[string]int{}
		}
		d.Types[m.Type]++
		d.Confidence[min(int(m.Confidence*10), len(d.Confidence)-1)]++
		confidences = append(confidences, m.Confidence)
	}
	d.MedianConfidence = medianOf(confidences)
	return d
}

// medianOf returns the median of xs, or nil when xs is empty. xs is sorted in
// place.
func medianOf(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	slices.Sort(xs)
	m := xs[len(xs)/2]
	if len(xs)%2 == 0 {
		m = (xs[len(xs)/2-1] + m) / 2
	}
	return &m
}

// sameYardstick fails when baseline was scored with a profile file other than
// cur's, naming the first control or reference that differs.
func sameYardstick(baseline, cur *Report) error {
	bp, cp := baseline.Provenance.Profile, cur.Provenance.Profile
	if bp.Digest == cp.Digest && bp.CSVDigest == cp.CSVDigest {
		return nil
	}
	if bp.Digest == cp.Digest {
		return fmt.Errorf("baseline job %s was scored with references.csv digest %q, but this run uses digest %q: "+
			"score both runs with the same references.csv to compare them",
			baseline.Provenance.JobID, bp.CSVDigest, cp.CSVDigest)
	}
	return fmt.Errorf("baseline job %s was scored with profile %s digest %s, but this run uses profile %s digest %s: %s; "+
		"score both runs with the same profile file to compare them",
		baseline.Provenance.JobID, bp.Name, bp.Digest, cp.Name, cp.Digest, firstDifference(baseline, cur))
}

func firstDifference(baseline, cur *Report) string {
	bc, cc := baseline.Provenance.Profile.Controls, cur.Provenance.Profile.Controls
	for i := range max(len(bc), len(cc)) {
		b, c := nth(bc, i, quote), nth(cc, i, quote)
		if b != c {
			return fmt.Sprintf("control %d is %s in the baseline and %s now", i+1, b, c)
		}
	}
	refPair := func(r ScoredReference) string { return Pair{r.SourceID, r.TargetID}.String() }
	br, cr := baseline.References, cur.References
	for i := range max(len(br), len(cr)) {
		b, c := nth(br, i, refPair), nth(cr, i, refPair)
		if b != c {
			return fmt.Sprintf("reference %d is %s in the baseline and %s now", i+1, b, c)
		}
	}
	return "their controls and resolved references match, so the profile files differ elsewhere"
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

// nth formats s[i], or returns "(none)" past the end of s.
func nth[T any](s []T, i int, format func(T) string) string {
	if i >= len(s) {
		return "(none)"
	}
	return format(s[i])
}

// agreement compares cur, sorted by pair, with the baseline's related pairs.
// It is not independent when the relationship analyzer reused results from
// earlier jobs, which a repeat of the same configuration returns unchanged, and
// its independence is unknown (nil) unless rel is a comparable cost: without a
// trustworthy cache summary there is no evidence either way.
func agreement(baseline *Report, cur []Mapping, rel AnalyzerCost) *Agreement {
	a := &Agreement{
		BaselineLabel:  baseline.Provenance.Label,
		BaselineJobID:  baseline.Provenance.JobID,
		BaselineCommit: baseline.Provenance.Commit,
	}
	if rel.Comparable {
		independent := rel.Hits == 0
		a.Independent = &independent
		a.ReusedRelationshipResults = rel.Hits
	}
	prev := map[Pair]Mapping{}
	for _, m := range baseline.Related {
		prev[Pair{m.SourceID, m.TargetID}] = m
	}
	inCur := pairSet{}
	same := 0
	var deltas []float64
	for _, m := range cur {
		pr := Pair{m.SourceID, m.TargetID}
		inCur[pr] = true
		pm, ok := prev[pr]
		if !ok {
			a.OnlyCurrent = append(a.OnlyCurrent, pr)
			continue
		}
		a.Both++
		if pm.Type == m.Type {
			same++
		}
		deltas = append(deltas, math.Abs(m.Confidence-pm.Confidence))
	}
	for _, m := range baseline.Related {
		if pr := (Pair{m.SourceID, m.TargetID}); !inCur[pr] {
			a.OnlyBaseline = append(a.OnlyBaseline, pr)
		}
	}
	slices.SortFunc(a.OnlyBaseline, comparePairs)
	a.Jaccard = newRate(a.Both, a.Both+len(a.OnlyCurrent)+len(a.OnlyBaseline))
	a.SameType = newRate(same, a.Both)
	a.MedianAbsConfidenceDelta = medianOf(deltas)
	return a
}
