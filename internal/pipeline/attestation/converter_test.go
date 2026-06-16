package attestation_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	pkgattestation "github.com/complytime-labs/crosscodex/pkg/attestation"

	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
)

// stubAnalyzer implements analyzer.Analyzer[proto.Message] for testing.
type stubAnalyzer struct {
	name string
	deps []string
}

func (s *stubAnalyzer) Name() string       { return s.name }
func (s *stubAnalyzer) DependsOn() []string { return s.deps }
func (s *stubAnalyzer) GenerateWork(_ context.Context, _ proto.Message, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	return nil, nil
}
func (s *stubAnalyzer) Aggregate(_ context.Context, _ []analyzer.TaskResult) (*analyzer.Output, error) {
	return nil, nil
}
func (s *stubAnalyzer) ResultSchema() proto.Message { return nil }

func buildDAG(t *testing.T, analyzers ...*stubAnalyzer) *analyzer.DAG {
	t.Helper()
	reg := analyzer.NewRegistry()
	for _, a := range analyzers {
		if err := analyzer.Register[proto.Message](reg, a); err != nil {
			t.Fatalf("register %s: %v", a.name, err)
		}
	}
	dag, err := reg.BuildDAG(context.Background())
	if err != nil {
		t.Fatalf("build DAG: %v", err)
	}
	return dag
}

func TestConverter(t *testing.T) {
	tests := []struct {
		name      string
		analyzers []*stubAnalyzer
		wantSteps int
		checkFunc func(t *testing.T, opts pkgattestation.LayoutOptions)
	}{
		{
			name:      "empty DAG produces zero steps",
			analyzers: nil,
			wantSteps: 0,
		},
		{
			name: "single analyzer with no deps",
			analyzers: []*stubAnalyzer{
				{name: "ingestion", deps: nil},
			},
			wantSteps: 1,
			checkFunc: func(t *testing.T, opts pkgattestation.LayoutOptions) {
				t.Helper()
				s := opts.Steps[0]
				if s.Name != "ingestion" {
					t.Errorf("step name = %q, want %q", s.Name, "ingestion")
				}
				if len(s.ExpectedMaterials) != 0 {
					t.Errorf("expected no materials, got %v", s.ExpectedMaterials)
				}
				if len(s.ExpectedProducts) != 1 || s.ExpectedProducts[0] != "ingestion.output" {
					t.Errorf("expected products = [ingestion.output], got %v", s.ExpectedProducts)
				}
				if s.Threshold != 1 {
					t.Errorf("threshold = %d, want 1", s.Threshold)
				}
			},
		},
		{
			name: "two analyzers with dependency",
			analyzers: []*stubAnalyzer{
				{name: "ingestion", deps: nil},
				{name: "analysis", deps: []string{"ingestion"}},
			},
			wantSteps: 2,
			checkFunc: func(t *testing.T, opts pkgattestation.LayoutOptions) {
				t.Helper()
				var analysisStep *pkgattestation.Step
				for i := range opts.Steps {
					if opts.Steps[i].Name == "analysis" {
						analysisStep = &opts.Steps[i]
						break
					}
				}
				if analysisStep == nil {
					t.Fatal("analysis step not found")
				}
				if len(analysisStep.ExpectedMaterials) != 1 || analysisStep.ExpectedMaterials[0] != "ingestion.output" {
					t.Errorf("analysis materials = %v, want [ingestion.output]", analysisStep.ExpectedMaterials)
				}
				if len(analysisStep.ExpectedProducts) != 1 || analysisStep.ExpectedProducts[0] != "analysis.output" {
					t.Errorf("analysis products = %v, want [analysis.output]", analysisStep.ExpectedProducts)
				}
			},
		},
		{
			name: "multi-level DAG preserves level ordering",
			analyzers: []*stubAnalyzer{
				{name: "ingestion", deps: nil},
				{name: "analysis", deps: []string{"ingestion"}},
				{name: "synthesis", deps: []string{"analysis"}},
			},
			wantSteps: 3,
			checkFunc: func(t *testing.T, opts pkgattestation.LayoutOptions) {
				t.Helper()
				names := make([]string, len(opts.Steps))
				for i, s := range opts.Steps {
					names[i] = s.Name
				}
				ingIdx, anaIdx, synIdx := -1, -1, -1
				for i, n := range names {
					switch n {
					case "ingestion":
						ingIdx = i
					case "analysis":
						anaIdx = i
					case "synthesis":
						synIdx = i
					}
				}
				if ingIdx >= anaIdx || anaIdx >= synIdx {
					t.Errorf("wrong order: ingestion=%d analysis=%d synthesis=%d", ingIdx, anaIdx, synIdx)
				}
			},
		},
		{
			name: "ExpiresIn is zero (caller responsibility)",
			analyzers: []*stubAnalyzer{
				{name: "step-a", deps: nil},
			},
			wantSteps: 1,
			checkFunc: func(t *testing.T, opts pkgattestation.LayoutOptions) {
				t.Helper()
				if opts.ExpiresIn != 0 {
					t.Errorf("ExpiresIn = %v, want 0", opts.ExpiresIn)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dag *analyzer.DAG
			if len(tt.analyzers) == 0 {
				reg := analyzer.NewRegistry()
				var err error
				dag, err = reg.BuildDAG(context.Background())
				if err != nil {
					t.Fatalf("build empty DAG: %v", err)
				}
			} else {
				dag = buildDAG(t, tt.analyzers...)
			}

			conv := pipelineattestation.NewConverter()
			opts := conv.Convert(dag)

			if len(opts.Steps) != tt.wantSteps {
				t.Fatalf("steps count = %d, want %d", len(opts.Steps), tt.wantSteps)
			}

			if tt.checkFunc != nil {
				tt.checkFunc(t, opts)
			}
		})
	}
}
