package memdriver

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// normalizeValue converts a property value to the type Apache AGE returns
// when it is read back. It mirrors agedriver's cypherValue: strings, bools
// and float64 round-trip unchanged; float32 is written with %g (its shortest
// float32 decimal) and read back as float64; int and int64 are read back as
// float64 (JSON numbers); every other type, including slices, is written as
// its %v string.
func normalizeValue(v any) any {
	switch val := v.(type) {
	case string, bool, float64:
		return val
	case float32:
		s := strconv.FormatFloat(float64(val), 'g', -1, 32)
		// FormatFloat's own output (including "NaN", "+Inf", "-Inf") always
		// parses back with ParseFloat, so the error is always nil here.
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case int:
		return float64(val)
	case int64:
		return float64(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// nodeProps encodes a node the way agedriver's nodeToAGProperties writes it.
func nodeProps(n graphdb.Node) map[string]any {
	props := map[string]any{
		"id":         n.ID,
		"valid_from": graphdb.FormatTime(n.ValidFrom),
	}
	if n.ValidTo != nil {
		props["valid_to"] = graphdb.FormatTime(*n.ValidTo)
	}
	if n.CreatedBy != "" {
		props["created_by"] = n.CreatedBy
	}
	if n.CreationMethod != "" {
		props["creation_method"] = n.CreationMethod
	}
	for k, v := range n.Properties {
		props[k] = normalizeValue(v)
	}
	return props
}

// edgeProps encodes an edge the way agedriver's edgeToAGProperties writes it.
func edgeProps(e graphdb.Edge) map[string]any {
	props := map[string]any{
		"id":         e.ID,
		"valid_from": graphdb.FormatTime(e.ValidFrom),
	}
	if e.ValidTo != nil {
		props["valid_to"] = graphdb.FormatTime(*e.ValidTo)
	}
	if e.DeterminedBy != "" {
		props["determined_by"] = e.DeterminedBy
	}
	if e.DeterminationType != "" {
		props["determination_type"] = e.DeterminationType
	}
	if e.Confidence != 0 {
		props["confidence"] = e.Confidence
	}
	if e.Supersedes != "" {
		props["supersedes"] = e.Supersedes
	}
	for k, v := range e.Properties {
		props[k] = normalizeValue(v)
	}
	return props
}

// requiresProps encodes a REQUIRES edge the way agedriver's
// CreateRequiresEdge writes it.
func requiresProps(r graphdb.RequiresEdge) map[string]any {
	props := map[string]any{
		"id":                r.EdgeID(),
		"valid_from":        graphdb.FormatTime(r.AnalyzedAt),
		"confidence":        r.Confidence,
		"unanimous":         r.Unanimous,
		"valid_votes":       float64(r.ValidVotes),
		"total_votes":       float64(r.TotalVotes),
		"vote_weight":       r.VoteWeight,
		"samples_per_model": float64(r.SamplesPerModel),
		"analyzed_at":       graphdb.FormatTime(r.AnalyzedAt),
		"job_id":            r.JobID,
	}
	if len(r.Models) > 0 {
		models := make([]any, len(r.Models))
		for i, m := range r.Models {
			models[i] = m
		}
		props["models"] = models
	}
	if r.PromptVersion != "" {
		props["prompt_version"] = r.PromptVersion
	}
	return props
}

// copyProps deep-copies a stored property map so callers cannot mutate
// stored state. Stored values are scalars or []any of strings, so cloning
// the slices is a full deep copy.
func copyProps(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if list, ok := v.([]any); ok {
			v = slices.Clone(list)
		}
		dst[k] = v
	}
	return dst
}

func stringProp(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

func floatProp(props map[string]any, key string) float64 {
	f, _ := props[key].(float64)
	return f
}

// timeProp mirrors agedriver's extractTime: a missing or unparseable
// timestamp reads back as the zero time.
func timeProp(props map[string]any, key string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, stringProp(props, key))
	if err != nil {
		return time.Time{}
	}
	return t
}

// timePtrProp mirrors agedriver's extractTimePtr.
func timePtrProp(props map[string]any, key string) *time.Time {
	s := stringProp(props, key)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}
