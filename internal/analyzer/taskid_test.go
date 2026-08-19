package analyzer_test

import (
	"testing"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

func TestStripTaskIDPrefix(t *testing.T) {
	rest, ok := intanalyzer.StripTaskIDPrefix("classify-AC-1", "classify")
	if !ok || rest != "AC-1" {
		t.Fatalf("got (%q, %v), want (\"AC-1\", true)", rest, ok)
	}
	if _, ok := intanalyzer.StripTaskIDPrefix("other-AC-1", "classify"); ok {
		t.Fatalf("expected ok=false for mismatched prefix")
	}
}

func TestParsePairTaskID(t *testing.T) {
	models := []string{"llama3.2:3b", "qwen3:8b"}
	source, target, model, sample, ok := intanalyzer.ParsePairTaskID(
		"relationship-AC-1--SC-7(2)-qwen3:8b-s2", "relationship", models)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if source != "AC-1" || target != "SC-7(2)" || model != "qwen3:8b" || sample != 2 {
		t.Fatalf("got (%q,%q,%q,%d)", source, target, model, sample)
	}
}

func TestParseControlTaskID(t *testing.T) {
	models := []string{"llama3.2:3b"}
	controlID, model, sample, skipped, ok := intanalyzer.ParseControlTaskID(
		"artifacts-AC-1-llama3.2:3b-s0", "artifacts", models)
	if !ok || skipped || controlID != "AC-1" || model != "llama3.2:3b" || sample != 0 {
		t.Fatalf("got (%q,%q,%d,%v,%v)", controlID, model, sample, skipped, ok)
	}

	controlID, _, _, skipped, ok = intanalyzer.ParseControlTaskID(
		"artifacts-AC-1-skip", "artifacts", models)
	if !ok || !skipped || controlID != "AC-1" {
		t.Fatalf("skip case: got (%q, skipped=%v, ok=%v)", controlID, skipped, ok)
	}
}

func TestParsePairTaskID_Failures(t *testing.T) {
	models := []string{"llama3.2:3b", "qwen3:8b"}
	cases := []struct {
		name   string
		taskID string
	}{
		{"missing prefix", "other-AC-1--AC-2-qwen3:8b-s0"},
		{"missing pairwise separator", "relationship-AC-1-AC-2-qwen3:8b-s0"},
		{"no sample suffix", "relationship-AC-1--AC-2-qwen3:8b"},
		{"model not in configured list", "relationship-AC-1--AC-2-unknown-model-s0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, ok := intanalyzer.ParsePairTaskID(tc.taskID, "relationship", models)
			if ok {
				t.Fatalf("expected ok=false for %q", tc.taskID)
			}
		})
	}
}

func TestParseControlTaskID_Failures(t *testing.T) {
	models := []string{"llama3.2:3b"}
	cases := []struct {
		name   string
		taskID string
	}{
		{"missing prefix", "other-AC-1-llama3.2:3b-s0"},
		{"no sample suffix and not a skip", "artifacts-AC-1-llama3.2:3b"},
		{"model not in configured list", "artifacts-AC-1-unknown-model-s0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, ok := intanalyzer.ParseControlTaskID(tc.taskID, "artifacts", models)
			if ok {
				t.Fatalf("expected ok=false for %q", tc.taskID)
			}
		})
	}
}
