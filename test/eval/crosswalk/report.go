package crosswalk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// Rate is hit out of total. Rate is hit/total, and 0 when total is 0, so a
// report never holds NaN.
type Rate struct {
	Hit   int     `json:"hit"`
	Total int     `json:"total"`
	Rate  float64 `json:"rate"`
}

func newRate(hit, total int) Rate {
	r := Rate{Hit: hit, Total: total}
	if total > 0 {
		r.Rate = float64(hit) / float64(total)
	}
	return r
}

func (r Rate) String() string {
	return fmt.Sprintf("%d/%d (%.1f%%)", r.Hit, r.Total, 100*r.Rate)
}

// Report is the score of one run against one profile. Its JSON form
// (report.json) is self-sufficient as the next run's baseline.
type Report struct {
	Provenance Provenance `json:"provenance"`
	// Resolution counts distinct (source control, identifier) references:
	// those resolving to exactly one target control, out of all of them
	// (resolved, unresolved and ambiguous).
	Resolution Rate `json:"resolution"`
	// References are the resolved references, the recall denominators, in
	// input order.
	References   []ScoredReference     `json:"references"`
	Unresolved   []UnresolvedReference `json:"unresolved"`
	Ambiguous    []UnresolvedReference `json:"ambiguous"`
	Recall       Recall                `json:"recall"`
	Cost         Cost                  `json:"cost"`
	Distribution Distribution          `json:"distribution"`
	// Related is every related mapping of the run, sorted by source then
	// target: the pair set a later run's agreement is computed against.
	Related []Mapping `json:"related"`
	// Agreement is nil without a baseline.
	Agreement *Agreement `json:"agreement"`
}

// Provenance names the exact inputs a report was computed from.
type Provenance struct {
	Label  string `json:"label"`
	Commit string `json:"commit"`
	JobID  string `json:"job_id"`
	// SourceCatalogID and TargetCatalogID are the catalog IDs the results
	// export's analysis names.
	SourceCatalogID string          `json:"source_catalog_id"`
	TargetCatalogID string          `json:"target_catalog_id"`
	Profile         ProfileIdentity `json:"profile"`
	// The digests are hex SHA-256 of the catalog files.
	SourceDigest string `json:"source_digest"`
	SubsetDigest string `json:"subset_digest"`
	TargetDigest string `json:"target_digest"`
}

// ProfileIdentity is the yardstick a report was scored with.
type ProfileIdentity struct {
	Name     string   `json:"name"`
	Digest   string   `json:"digest"`
	Controls []string `json:"controls"`
	// CSVDigest is the hex SHA-256 of the references.csv the profile names;
	// empty when the profile uses oscal_links.
	CSVDigest string `json:"csv_digest,omitempty"`
}

// ScoredReference is a resolved reference and how the run did on it.
type ScoredReference struct {
	SourceID       string `json:"source_id"`
	TargetID       string `json:"target_id"`
	Citation       string `json:"citation"`
	FoundExact     bool   `json:"found_exact"`
	FoundBase      bool   `json:"found_base"`
	ReachableExact bool   `json:"reachable_exact"`
	ReachableBase  bool   `json:"reachable_base"`
}

// UnresolvedReference is a reference identifier that names no single target
// control. Candidates lists the matching controls of an ambiguous one.
type UnresolvedReference struct {
	SourceID   string   `json:"source_id"`
	Identifier string   `json:"identifier"`
	Citation   string   `json:"citation"`
	Candidates []string `json:"candidates,omitempty"`
}

// Recall counts references found by related mappings and reachable through
// candidates, exactly and by base control.
type Recall struct {
	FoundExact     Rate `json:"found_exact"`
	FoundBase      Rate `json:"found_base"`
	ReachableExact Rate `json:"reachable_exact"`
	ReachableBase  Rate `json:"reachable_base"`
	// ByGenerator is reachability through each generator's candidates alone;
	// candidates without contributions count under "unattributed".
	ByGenerator map[string]Reachability `json:"reachable_by_generator"`
}

// Reachability is exact and base reachability.
type Reachability struct {
	Exact Rate `json:"exact"`
	Base  Rate `json:"base"`
}

// Cost is the run's wall time and analysis calls.
type Cost struct {
	Stages []StageCost `json:"stages"`
	// TotalSeconds runs from the first stage start to the last completion;
	// null when there are no stages or any stage is incomplete.
	TotalSeconds *float64       `json:"total_seconds"`
	Analyzers    []AnalyzerCost `json:"analyzers"`
	// RelationshipReuse is null when the job has no relationship analyzer.
	RelationshipReuse *Reuse `json:"relationship_reuse"`
}

// embeddingAnalyzer is the analyzer whose calls go to the embedding model, not
// a chat model.
const embeddingAnalyzer = "embedding"

// ChatCalls is the number of chat calls this run sent: Dispatched summed over
// every analyzer except embedding. It reports false when there are no analyzers
// or any analyzer's figure is not comparable, so a partial sum is never shown.
func (c Cost) ChatCalls() (int64, bool) {
	if len(c.Analyzers) == 0 {
		return 0, false
	}
	var total int64
	for _, a := range c.Analyzers {
		if !a.Comparable {
			return 0, false
		}
		if a.Analyzer != embeddingAnalyzer {
			total += a.Dispatched
		}
	}
	return total, true
}

// Reuse is how many of an analyzer's results (Hits, out of Tasks) came from
// earlier jobs through the result cache.
type Reuse struct {
	Hits  int64 `json:"hits"`
	Tasks int64 `json:"tasks"`
}

// StageCost is one stage's wall time; Seconds is null when the stage has no
// start or no completion.
type StageCost struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	Seconds     *float64  `json:"seconds"`
}

// Distribution describes the related mappings.
type Distribution struct {
	// Types counts related mappings by relationship type name.
	Types map[string]int `json:"types"`
	// Confidence[i] counts confidences in [i/10, (i+1)/10); the last bucket
	// also holds 1.0.
	Confidence [10]int `json:"confidence_buckets"`
	// MedianConfidence is null without related mappings.
	MedianConfidence *float64 `json:"median_confidence"`
}

// Pair is a (source, target) control pair.
type Pair struct {
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
}

// Agreement compares the run's related pair set with a baseline's.
type Agreement struct {
	BaselineLabel  string `json:"baseline_label"`
	BaselineJobID  string `json:"baseline_job_id"`
	BaselineCommit string `json:"baseline_commit"`
	Both           int    `json:"both"`
	OnlyCurrent    []Pair `json:"only_current"`
	OnlyBaseline   []Pair `json:"only_baseline"`
	// Jaccard is both out of the union.
	Jaccard Rate `json:"jaccard"`
	// SameType is shared pairs with the same type, out of shared pairs.
	SameType Rate `json:"same_type"`
	// MedianAbsConfidenceDelta is over shared pairs; null when none.
	MedianAbsConfidenceDelta *float64 `json:"median_abs_confidence_delta"`
	// Independent is false when the current run reused relationship results
	// from earlier jobs (ReusedRelationshipResults of them), so agreement
	// partly compares a cache with itself. It is null (unknown) unless the
	// relationship analyzer's cache summary is comparable.
	Independent               *bool `json:"independent"`
	ReusedRelationshipResults int64 `json:"reused_relationship_results"`
}

// LoadReport reads a report.json written from a Report, rejecting unknown
// fields so a file from an incompatible version is not half-read.
func LoadReport(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading report %q: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding report %q: %w", path, err)
	}
	if r.Provenance.Profile.Digest == "" {
		return nil, fmt.Errorf("report %q has no profile digest: pass a report.json written by the crosswalk score command", path)
	}
	return &r, nil
}

// cell makes untrusted text safe inside a Markdown table cell.
func cell(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "|", `\|`).Replace(s)
}

func seconds(s *float64) string {
	if s == nil {
		return "incomplete"
	}
	return fmt.Sprintf("%.1f s", *s)
}

func median(m *float64) string {
	if m == nil {
		return "none"
	}
	return fmt.Sprintf("%.3f", *m)
}

// Markdown renders the report for people: resolution first, then recall,
// reachability, cost, distributions, agreement and provenance.
func (r *Report) Markdown() string {
	var b strings.Builder
	p := r.Provenance
	fmt.Fprintf(&b, "# Crosswalk eval: %s\n\n", cell(p.Profile.Name))
	fmt.Fprintf(&b, "Resolution: %s of distinct (source control, identifier) references resolve to exactly one target control.\n\n", r.Resolution)

	b.WriteString("## Recall\n\n| | Exact | Base |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Found | %s | %s |\n", r.Recall.FoundExact, r.Recall.FoundBase)
	fmt.Fprintf(&b, "| Reachable | %s | %s |\n", r.Recall.ReachableExact, r.Recall.ReachableBase)
	for _, g := range slices.Sorted(maps.Keys(r.Recall.ByGenerator)) {
		x := r.Recall.ByGenerator[g]
		fmt.Fprintf(&b, "| Reachable via %s | %s | %s |\n", cell(g), x.Exact, x.Base)
	}

	b.WriteString("\n## Cost\n\n| Stage | Status | Wall time |\n|---|---|---|\n")
	for _, s := range r.Cost.Stages {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(s.Name), cell(s.Status), seconds(s.Seconds))
	}
	fmt.Fprintf(&b, "\nTotal wall time: %s\n\n", seconds(r.Cost.TotalSeconds))
	if u := r.Cost.RelationshipReuse; u == nil {
		b.WriteString("Cache reuse: no relationship analyzer in this job.\n\n")
	} else {
		fmt.Fprintf(&b, "Cache reuse: relationship reused %d of %d results from earlier jobs.\n\n", u.Hits, u.Tasks)
	}
	b.WriteString("| Analyzer | Cache mode | Tasks | Hits | Dispatched | Models |\n|---|---|---|---|---|---|\n")
	for _, a := range r.Cost.Analyzers {
		dispatched := fmt.Sprint(a.Dispatched)
		switch {
		case a.Comparable:
		case a.Attestation != attested:
			dispatched = "not comparable: unattested"
		case a.Mode == "":
			dispatched = "not comparable: cache mode (none)"
		default:
			dispatched = "not comparable: cache mode " + cell(a.Mode)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s | %s |\n",
			cell(a.Analyzer), cell(a.Mode), a.Tasks, a.Hits, dispatched, cell(strings.Join(a.Models, ", ")))
	}

	b.WriteString("\n## Distributions\n\n| Type | Mappings |\n|---|---|\n")
	for _, t := range slices.Sorted(maps.Keys(r.Distribution.Types)) {
		fmt.Fprintf(&b, "| %s | %d |\n", cell(t), r.Distribution.Types[t])
	}
	b.WriteString("\n| Confidence | Mappings |\n|---|---|\n")
	for i, n := range r.Distribution.Confidence {
		closing := ")"
		if i == len(r.Distribution.Confidence)-1 {
			closing = "]"
		}
		fmt.Fprintf(&b, "| [%.1f, %.1f%s | %d |\n", float64(i)/10, float64(i+1)/10, closing, n)
	}
	fmt.Fprintf(&b, "\nMedian confidence: %s\n", median(r.Distribution.MedianConfidence))

	b.WriteString("\n## Agreement\n\n")
	if a := r.Agreement; a == nil {
		b.WriteString("No baseline.\n")
	} else {
		fmt.Fprintf(&b, "Baseline: %s (job %s, commit %s)\n\n", cell(a.BaselineLabel), cell(a.BaselineJobID), cell(a.BaselineCommit))
		switch {
		case a.Independent == nil:
			b.WriteString("**independence unknown: the relationship analyzer's cache summary is missing or not comparable, so reuse of earlier results cannot be ruled out**\n\n")
		case !*a.Independent:
			fmt.Fprintf(&b, "**not independent: %d relationship results were reused from earlier jobs; rerun with NO_CACHE=1 to measure run-to-run variance**\n\n",
				a.ReusedRelationshipResults)
		}
		fmt.Fprintf(&b, "| Both | Only current | Only baseline | Jaccard | Same type | Median abs. confidence change |\n|---|---|---|---|---|---|\n")
		fmt.Fprintf(&b, "| %d | %d | %d | %s | %s | %s |\n",
			a.Both, len(a.OnlyCurrent), len(a.OnlyBaseline), a.Jaccard, a.SameType, median(a.MedianAbsConfidenceDelta))
	}

	b.WriteString("\n## References not resolved\n\n| Source | Identifier | Citation | Matches |\n|---|---|---|---|\n")
	for _, u := range append(append([]UnresolvedReference{}, r.Unresolved...), r.Ambiguous...) {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", cell(u.SourceID), cell(u.Identifier), cell(u.Citation), cell(strings.Join(u.Candidates, ", ")))
	}

	b.WriteString("\n## Provenance\n\n")
	items := [][2]string{
		{"Label", p.Label}, {"Commit", p.Commit}, {"Job", p.JobID},
		{"Source catalog ID", p.SourceCatalogID}, {"Target catalog ID", p.TargetCatalogID},
		{"Profile", p.Profile.Name}, {"Profile SHA-256", p.Profile.Digest},
		{"Source SHA-256", p.SourceDigest}, {"Subset SHA-256", p.SubsetDigest}, {"Target SHA-256", p.TargetDigest},
	}
	if p.Profile.CSVDigest != "" {
		items = append(items, [2]string{"References CSV SHA-256", p.Profile.CSVDigest})
	}
	for _, kv := range items {
		fmt.Fprintf(&b, "- %s: %s\n", kv[0], cell(kv[1]))
	}
	return b.String()
}
