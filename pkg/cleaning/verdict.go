package cleaning

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

const (
	// maxKeyPartBytes bounds each part of a verdict key: job and node IDs are
	// far shorter, so a longer part is malformed or hostile input.
	maxKeyPartBytes = 256
	// maxEvidenceBytes bounds stored evidence JSON so a corrupt or hostile
	// column value cannot force a large allocation.
	maxEvidenceBytes = 1 << 20
	// keySeparator joins the parts of a verdict key's CLI form.
	keySeparator = ":"
	// agreementProperty is the SEMANTIC_MATCH edge property holding the
	// panel's agreeing fraction of votes.
	agreementProperty = "agreement_fraction"
)

// ErrInvalidVerdictKey marks a verdict key that is not three non-empty
// ':'-separated parts of at most 256 bytes each.
var ErrInvalidVerdictKey = errors.New("invalid verdict key")

// ErrInvalidEvidence marks stored evidence that is malformed, carries unknown
// fields or invalid verdict keys, or exceeds 1 MiB.
var ErrInvalidEvidence = errors.New("invalid evidence")

// Signal names a cleaning signal. Values match the config keys under
// cleaning.signals.
type Signal string

const (
	SignalEquivalenceFanout     Signal = "equivalence_fanout"
	SignalCrossJobContradiction Signal = "cross_job_contradiction"
	SignalLowAgreement          Signal = "low_agreement"
	SignalConflictsWith         Signal = "conflicts_with"
	SignalStalePrompt           Signal = "stale_prompt"
	SignalStaleModel            Signal = "stale_model"
)

// VerdictKey identifies one job's verdict on a control pair in the stored
// edge direction. A verdict owns every edge of that job and direction, in
// both stages.
type VerdictKey struct {
	JobID    string `json:"job_id"`
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
}

// String returns the CLI form "<job>:<source>:<target>".
func (k VerdictKey) String() string {
	return k.JobID + keySeparator + k.SourceID + keySeparator + k.TargetID
}

func (k VerdictKey) validate() error {
	for _, part := range []struct{ name, value string }{
		{"job", k.JobID}, {"source", k.SourceID}, {"target", k.TargetID},
	} {
		if err := validateKeyPart(part.name, part.value); err != nil {
			return err
		}
	}
	return nil
}

// validateKeyPart applies a verdict key's rules for one part to value; a
// job-level finding's job ID follows the job part's rules.
func validateKeyPart(name, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: %s part is empty", ErrInvalidVerdictKey, name)
	case len(value) > maxKeyPartBytes:
		return fmt.Errorf("%w: %s part exceeds %d bytes", ErrInvalidVerdictKey, name, maxKeyPartBytes)
	case strings.Contains(value, keySeparator):
		return fmt.Errorf("%w: %s part contains the separator '%s'", ErrInvalidVerdictKey, name, keySeparator)
	}
	return nil
}

func compareKeys(x, y VerdictKey) int {
	return cmp.Or(cmp.Compare(x.JobID, y.JobID), cmp.Compare(x.SourceID, y.SourceID), cmp.Compare(x.TargetID, y.TargetID))
}

// ParseVerdictKey parses the CLI form "<job>:<source>:<target>". Anything but
// exactly three non-empty parts of at most 256 bytes each wraps
// ErrInvalidVerdictKey.
func ParseVerdictKey(s string) (VerdictKey, error) {
	parts := strings.Split(s, keySeparator)
	if len(parts) != 3 {
		return VerdictKey{}, fmt.Errorf("%w: want <job>:<source>:<target>, got %d ':'-separated parts", ErrInvalidVerdictKey, len(parts))
	}
	k := VerdictKey{JobID: parts[0], SourceID: parts[1], TargetID: parts[2]}
	if err := k.validate(); err != nil {
		return VerdictKey{}, err
	}
	return k, nil
}

// JobLevel reports whether s is raised per job rather than per verdict:
// stale_prompt and stale_model judge how a job ran, not what it concluded.
func (s Signal) JobLevel() bool { return s == SignalStalePrompt || s == SignalStaleModel }

// Evidence is what a finding shows a reviewer: the concerned verdicts in
// readable form, or for a job-level finding the job, and signal-specific
// detail.
type Evidence struct {
	Verdicts []VerdictEvidence `json:"verdicts"`
	// Job is set only on a job-level finding, which lists no Verdicts.
	Job    *JobEvidence      `json:"job,omitempty"`
	Detail map[string]string `json:"detail,omitempty"`
}

// JobEvidence is a job-level finding's subject. Its size does not grow with
// the job's verdicts: it counts them instead of listing them.
type JobEvidence struct {
	ID          string    `json:"id"`
	CompletedAt time.Time `json:"completed_at"`
	// SourceCatalog and TargetCatalog are the job's recorded
	// "<name> <version>", else the catalog ID.
	SourceCatalog string `json:"source_catalog"`
	TargetCatalog string `json:"target_catalog"`
	// VerdictsInScope counts the job's live verdicts the run read.
	VerdictsInScope int `json:"verdicts_in_scope"`
}

// VerdictEvidence is one verdict as a reviewer reads it, in the stored edge
// direction. Catalogs are the job's recorded "<name> <version>", else the
// catalog ID.
type VerdictEvidence struct {
	Key              VerdictKey `json:"key"`
	JobCompletedAt   time.Time  `json:"job_completed_at"`
	SourceIdentifier string     `json:"source_identifier"`
	SourceLabel      string     `json:"source_label"`
	SourceTitle      string     `json:"source_title"`
	SourceCatalog    string     `json:"source_catalog"`
	TargetIdentifier string     `json:"target_identifier"`
	TargetLabel      string     `json:"target_label"`
	TargetTitle      string     `json:"target_title"`
	TargetCatalog    string     `json:"target_catalog"`
	Relationship     string     `json:"relationship"`
	Agreement        *float64   `json:"agreement"`
	EdgeIDs          []string   `json:"edge_ids"`
}

// ParseEvidence strictly decodes stored evidence JSON. Input over 1 MiB,
// unknown fields, trailing data, and verdict keys or a job ID ParseVerdictKey
// would reject wrap ErrInvalidEvidence.
func ParseEvidence(b []byte) (Evidence, error) {
	if len(b) > maxEvidenceBytes {
		return Evidence{}, fmt.Errorf("%w: input exceeds %d bytes", ErrInvalidEvidence, maxEvidenceBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Evidence
	if err := dec.Decode(&e); err != nil {
		return Evidence{}, fmt.Errorf("%w: %v", ErrInvalidEvidence, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Evidence{}, fmt.Errorf("%w: unexpected data after the evidence", ErrInvalidEvidence)
	}
	for i, v := range e.Verdicts {
		if err := v.Key.validate(); err != nil {
			return Evidence{}, fmt.Errorf("%w: verdict %d: %v", ErrInvalidEvidence, i, err)
		}
	}
	if e.Job != nil {
		if err := validateKeyPart("job", e.Job.ID); err != nil {
			return Evidence{}, fmt.Errorf("%w: %v", ErrInvalidEvidence, err)
		}
	}
	return e, nil
}

// Job is a completed job as detection needs it.
type Job struct {
	ID          string
	CompletedAt time.Time
	Catalogs    jobconfig.Catalogs
	// Stages maps a stage name to its recorded prompt and models; a nil
	// record (or a missing stage) is unknown, never stale.
	Stages map[string]*StageRecord
}

// Verdict is one job's judgement of a pair in one stored direction, joined
// across the analyzer and synthesized stages.
type Verdict struct {
	Key         VerdictKey
	CompletedAt time.Time
	// Type and Confidence come from the analyzer-stage edge, else the
	// synthesized one. Confidence only breaks ties in crosswalk.Merge.
	Type       pb.RelationshipType
	Confidence float64
	// Reversed is true when the stored direction is the pair's B to A.
	Reversed bool
	// Agreement is the first parsable agreement_fraction of its edges,
	// analyzer stage first; nil when absent, unparsable or not finite.
	Agreement *float64
	// EdgeIDs are the verdict's edges in both stages, sorted.
	EdgeIDs        []string
	Source, Target graphdb.Node // stored direction
}

// Pair holds every verdict on one unordered control pair.
type Pair struct {
	A, B     graphdb.Node
	Verdicts []Verdict
}

// BuildPairs joins the analyzer-stage and synthesized-stage verdicts that
// crosswalk.LoadPair returned for the same catalogs A and B, so that one
// VerdictKey owns its edges of both stages. Pairs are sorted by (A.ID, B.ID)
// and each pair's verdicts by key.
func BuildPairs(analyzer, synthesized []crosswalk.PairEdges) []Pair {
	type pairKey struct{ a, b string }
	pairs := map[pairKey]*Pair{}
	verdicts := map[VerdictKey]*Verdict{}

	// Analyzer edges are read first, so the edge that creates a verdict
	// supplies its Type and Confidence from the analyzer stage when it has one.
	for _, stage := range [][]crosswalk.PairEdges{analyzer, synthesized} {
		for _, pe := range stage {
			pk := pairKey{pe.A.ID, pe.B.ID}
			p := pairs[pk]
			if p == nil {
				p = &Pair{A: pe.A, B: pe.B}
				pairs[pk] = p
			}
			for _, ev := range pe.Verdicts {
				k := VerdictKey{JobID: ev.JobID, SourceID: ev.SourceID, TargetID: ev.TargetID}
				v := verdicts[k]
				if v == nil {
					src, tgt := pe.A, pe.B
					if ev.Reversed {
						src, tgt = tgt, src
					}
					v = &Verdict{
						Key: k, CompletedAt: ev.CompletedAt, Type: ev.Type, Confidence: ev.Confidence,
						Reversed: ev.Reversed, Source: src, Target: tgt,
					}
					verdicts[k] = v
					p.Verdicts = append(p.Verdicts, Verdict{Key: k})
				}
				if !slices.Contains(v.EdgeIDs, ev.Edge.ID) {
					v.EdgeIDs = append(v.EdgeIDs, ev.Edge.ID)
				}
				if v.Agreement == nil {
					v.Agreement = parseAgreement(ev.Edge.Properties[agreementProperty])
				}
			}
		}
	}

	out := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		for i := range p.Verdicts {
			v := verdicts[p.Verdicts[i].Key]
			slices.Sort(v.EdgeIDs)
			p.Verdicts[i] = *v
		}
		slices.SortFunc(p.Verdicts, func(x, y Verdict) int { return compareKeys(x.Key, y.Key) })
		out = append(out, *p)
	}
	slices.SortFunc(out, func(x, y Pair) int {
		return cmp.Or(cmp.Compare(x.A.ID, y.A.ID), cmp.Compare(x.B.ID, y.B.ID))
	})
	return out
}

// parseAgreement reads an agreement_fraction property, stored as decimal
// text by the relationship analyzer or as a number by other writers.
func parseAgreement(raw any) *float64 {
	var f float64
	switch v := raw.(type) {
	case string:
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		f = parsed
	case float64:
		f = v
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// evidenceOf renders v for a reviewer, naming catalogs from job's recorded
// labels.
func evidenceOf(v Verdict, job Job) VerdictEvidence {
	return VerdictEvidence{
		Key:              v.Key,
		JobCompletedAt:   v.CompletedAt,
		SourceIdentifier: nodeString(v.Source, "identifier"),
		SourceLabel:      nodeString(v.Source, "label"),
		SourceTitle:      nodeString(v.Source, "title"),
		SourceCatalog:    catalogDisplay(job.Catalogs, nodeString(v.Source, "catalog_id")),
		TargetIdentifier: nodeString(v.Target, "identifier"),
		TargetLabel:      nodeString(v.Target, "label"),
		TargetTitle:      nodeString(v.Target, "title"),
		TargetCatalog:    catalogDisplay(job.Catalogs, nodeString(v.Target, "catalog_id")),
		Relationship:     crosswalk.Word(v.Type),
		Agreement:        v.Agreement,
		EdgeIDs:          slices.Clone(v.EdgeIDs),
	}
}

func catalogDisplay(cats jobconfig.Catalogs, catalogID string) string {
	label := cats.LabelOf(catalogID)
	if !label.Recorded {
		return catalogID
	}
	return strings.TrimSpace(label.Name + " " + label.Version)
}

func nodeString(n graphdb.Node, key string) string {
	s, _ := n.Properties[key].(string)
	return s
}
