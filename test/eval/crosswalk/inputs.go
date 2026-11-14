package crosswalk

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	// noRelationship is the relationship type that is not a related mapping.
	noRelationship = "RELATIONSHIP_TYPE_NO_RELATIONSHIP"
	// cacheEnabled and cacheBypassed are the run status cache modes in which
	// the run sent its own calls, so dispatched is this run's call count.
	cacheEnabled  = "enabled"
	cacheBypassed = "bypassed"
	// attested is the run status attestation of a complete cache summary.
	attested = "attested"
)

// Mapping is one related analyzer-stage mapping. Type is the protojson
// relationship enum name, e.g. "RELATIONSHIP_TYPE_EQUIVALENT".
type Mapping struct {
	SourceID   string  `json:"source_id"`
	TargetID   string  `json:"target_id"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

// Candidate is one candidate pair and the distinct generators that proposed
// it, in contribution order; empty when the export has no contributions.
type Candidate struct {
	SourceID   string
	TargetID   string
	Generators []string
}

// Stage is one job stage. A zero Started or Completed means unset.
type Stage struct {
	Name      string
	Status    string
	Started   time.Time
	Completed time.Time
}

// AnalyzerCost is one analyzer's result-cache summary. Dispatched is the
// number of calls this run sent only when Comparable: the summary is attested
// and Mode is "enabled" or "bypassed". Hits are results reused from earlier
// jobs. Models lists the distinct models that answered, each as the observed
// model with its deployment hash, which identifies the model beyond the alias
// the server reports.
type AnalyzerCost struct {
	Analyzer    string   `json:"analyzer"`
	Mode        string   `json:"mode"`
	Attestation string   `json:"attestation"`
	Tasks       int64    `json:"tasks"`
	Hits        int64    `json:"hits"`
	Dispatched  int64    `json:"dispatched"`
	Comparable  bool     `json:"comparable"`
	Models      []string `json:"models"`
}

// Run is one job's results as the CLI exported them. Analysis is the
// results export's "analysis" object, verbatim. Mappings holds related
// mappings only.
type Run struct {
	JobID      string
	Analysis   json.RawMessage
	Mappings   []Mapping
	Candidates []Candidate
	Stages     []Stage
	Costs      []AnalyzerCost
}

// mappingsFile is `crosscodex results export --format json`; mappings are
// protojson ControlMapping objects.
type mappingsFile struct {
	jobFile
	Analysis json.RawMessage `json:"analysis"`
	Mappings []struct {
		SourceControlID  string  `json:"sourceControlId"`
		TargetControlID  string  `json:"targetControlId"`
		RelationshipType string  `json:"relationshipType"`
		Confidence       float64 `json:"confidence"`
	} `json:"mappings"`
}

// candidatesFile is `crosscodex results candidates --format json`.
type candidatesFile struct {
	jobFile
	Candidates []struct {
		SourceControlID string `json:"sourceControlId"`
		TargetControlID string `json:"targetControlId"`
		Contributions   []struct {
			Generator string `json:"generator"`
		} `json:"contributions"`
	} `json:"candidates"`
}

// statusFile is `crosscodex run status --json`; timestamps are RFC 3339, ""
// when unset.
type statusFile struct {
	jobFile
	Stages []struct {
		Name        string `json:"name"`
		Status      string `json:"status"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"stages"`
	Cache []struct {
		Analyzer    string `json:"analyzer"`
		Attestation string `json:"attestation"`
		Mode        string `json:"mode"`
		Tasks       int64  `json:"tasks"`
		Hits        int64  `json:"hits"`
		Dispatched  int64  `json:"dispatched"`
		Models      []struct {
			ObservedModel string `json:"observed_model"`
			Deployment    string `json:"deployment"`
		} `json:"models"`
	} `json:"cache"`
}

// jobFile is the job_id every CLI JSON output above carries.
type jobFile struct {
	JobID string `json:"job_id"`
}

func (f jobFile) job() string { return f.JobID }

// decodeFile decodes the JSON file at path into v, rejecting a file without a
// job_id.
func decodeFile(path, what string, v interface{ job() string }) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s %q: %w", what, path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decoding %s %q: %w", what, path, err)
	}
	if v.job() == "" {
		return fmt.Errorf("%s %q has no job_id: pass the JSON output of the crosscodex CLI", what, path)
	}
	return nil
}

// bareControlID returns the control ID in a node ID the CLI exported as
// "<catalog-id>/<control-id>", requiring the prefix to be exactly the job's
// catalog ID: an ID without it belongs to another catalog or job, so it is an
// error rather than a guess.
func bareControlID(path, field, id, catalogID string) (string, error) {
	bare, ok := strings.CutPrefix(id, catalogID+"/")
	if !ok || bare == "" {
		return "", fmt.Errorf("%q: %s %q is not %q followed by a control ID: score the results export and candidates export of the same job",
			path, field, id, catalogID+"/")
	}
	return bare, nil
}

// LoadRun reads a job's analyzer-stage results export, candidates export and
// run status. All three must name the same job. Mappings of type
// RELATIONSHIP_TYPE_NO_RELATIONSHIP are dropped; every other type is kept.
// The CLI exports control IDs as "<catalog-id>/<control-id>"; the Run holds the
// bare control IDs, after checking the catalog IDs against the export's analysis.
func LoadRun(mappingsPath, candidatesPath, statusPath string) (*Run, error) {
	var m mappingsFile
	if err := decodeFile(mappingsPath, "results export", &m); err != nil {
		return nil, err
	}
	var c candidatesFile
	if err := decodeFile(candidatesPath, "candidates export", &c); err != nil {
		return nil, err
	}
	var s statusFile
	if err := decodeFile(statusPath, "run status", &s); err != nil {
		return nil, err
	}
	for _, other := range []struct{ path, job string }{{candidatesPath, c.JobID}, {statusPath, s.JobID}} {
		if other.job != m.JobID {
			return nil, fmt.Errorf("%q is for job %q but %q is for job %q: export the mappings, candidates and status of the same job",
				mappingsPath, m.JobID, other.path, other.job)
		}
	}

	run := &Run{JobID: m.JobID, Analysis: m.Analysis}
	for _, x := range m.Mappings {
		if x.Confidence < 0 || x.Confidence > 1 {
			return nil, fmt.Errorf("%q: mapping %s -> %s has confidence %v; it must be between 0 and 1",
				mappingsPath, x.SourceControlID, x.TargetControlID, x.Confidence)
		}
		if x.RelationshipType == noRelationship {
			continue
		}
		run.Mappings = append(run.Mappings, Mapping{
			SourceID: x.SourceControlID, TargetID: x.TargetControlID,
			Type: x.RelationshipType, Confidence: x.Confidence,
		})
	}
	for _, x := range c.Candidates {
		var gens []string
		for _, contrib := range x.Contributions {
			if !slices.Contains(gens, contrib.Generator) {
				gens = append(gens, contrib.Generator)
			}
		}
		run.Candidates = append(run.Candidates, Candidate{SourceID: x.SourceControlID, TargetID: x.TargetControlID, Generators: gens})
	}
	for _, x := range s.Stages {
		st := Stage{Name: x.Name, Status: x.Status}
		for _, ts := range []struct {
			field, value string
			dst          *time.Time
		}{{"started_at", x.StartedAt, &st.Started}, {"completed_at", x.CompletedAt, &st.Completed}} {
			if ts.value == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, ts.value)
			if err != nil {
				return nil, fmt.Errorf("%q: stage %q %s: %w", statusPath, x.Name, ts.field, err)
			}
			*ts.dst = t
		}
		run.Stages = append(run.Stages, st)
	}
	for _, x := range s.Cache {
		cost := AnalyzerCost{
			Analyzer: x.Analyzer, Mode: x.Mode, Attestation: x.Attestation, Tasks: x.Tasks, Hits: x.Hits,
			Dispatched: x.Dispatched,
			Comparable: x.Attestation == attested && (x.Mode == cacheEnabled || x.Mode == cacheBypassed),
		}
		for _, model := range x.Models {
			name := model.ObservedModel
			if name == "" {
				continue
			}
			if model.Deployment != "" {
				name += " (deployment " + model.Deployment + ")"
			}
			if !slices.Contains(cost.Models, name) {
				cost.Models = append(cost.Models, name)
			}
		}
		run.Costs = append(run.Costs, cost)
	}

	sourceCatalogID, targetCatalogID, err := analysisCatalogs(run)
	if err != nil {
		return nil, err
	}
	for i := range run.Mappings {
		x := &run.Mappings[i]
		if x.SourceID, err = bareControlID(mappingsPath, "sourceControlId", x.SourceID, sourceCatalogID); err != nil {
			return nil, err
		}
		if x.TargetID, err = bareControlID(mappingsPath, "targetControlId", x.TargetID, targetCatalogID); err != nil {
			return nil, err
		}
	}
	for i := range run.Candidates {
		x := &run.Candidates[i]
		if x.SourceID, err = bareControlID(candidatesPath, "sourceControlId", x.SourceID, sourceCatalogID); err != nil {
			return nil, err
		}
		if x.TargetID, err = bareControlID(candidatesPath, "targetControlId", x.TargetID, targetCatalogID); err != nil {
			return nil, err
		}
	}
	return run, nil
}
