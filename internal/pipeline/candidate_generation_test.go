package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// fakeControlsReader is a narrow ControlsReader test double.
type fakeControlsReader struct {
	controls map[string]*candidate.ControlData
	err      error
}

func (f fakeControlsReader) ControlData(_ context.Context, _, _, _ string) (map[string]*candidate.ControlData, error) {
	return f.controls, f.err
}

// fakeEmbeddingsReader is a narrow EmbeddingsReader test double.
type fakeEmbeddingsReader struct {
	ids    []string
	values [][]float32
	err    error
}

func (f fakeEmbeddingsReader) SimilarityMatrix(_ context.Context, _, _, _ string) ([]string, [][]float32, error) {
	return f.ids, f.values, f.err
}

// fakeCandidateWriter records what it was asked to write so tests can assert
// that nothing is persisted on the failure paths.
type fakeCandidateWriter struct {
	requiresCalled     bool
	relationshipCalled bool
	requiresPairs      []requires.RequiresPair
	relationshipPairs  []relationship.CandidatePair
	requiresErr        error
	relationshipErr    error
}

func (f *fakeCandidateWriter) WriteRequires(_ context.Context, _, _ string, pairs []requires.RequiresPair) error {
	f.requiresCalled = true
	f.requiresPairs = pairs
	return f.requiresErr
}

func (f *fakeCandidateWriter) WriteRelationship(_ context.Context, _, _ string, pairs []relationship.CandidatePair, _ map[string][]byte) error {
	f.relationshipCalled = true
	f.relationshipPairs = pairs
	return f.relationshipErr
}

// fixedGenerator is a candidate.Generator that returns a preset candidate set,
// letting tests exercise the persistence/conversion path with a known score.
type fixedGenerator struct {
	name       string
	candidates []candidate.Candidate
}

func (g fixedGenerator) Name() string { return g.name }

func (g fixedGenerator) Generate(_ context.Context, _ candidate.GenerateRequest) ([]candidate.Candidate, error) {
	return g.candidates, nil
}

var threeControls = map[string]*candidate.ControlData{
	"c1": {ControlID: "c1", Text: "control one"},
	"c2": {ControlID: "c2", Text: "control two"},
	"c3": {ControlID: "c3", Text: "control three"},
}

func TestCandidateGenerator_SkipsNonCatalogJobs(t *testing.T) {
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{}, fakeEmbeddingsReader{},
		candidate.NewRegistry(), &fakeCandidateWriter{},
		config.CandidateConfig{}, "test-model",
	)
	jobConfig := []byte(`{"Source":{"DocumentContent":"..."}}`)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", jobConfig)
	if !errors.Is(err, pipeline.ErrNotACatalogJob) {
		t.Fatalf("expected ErrNotACatalogJob, got %v", err)
	}
}

func TestCandidateGenerator_ExtractsCatalogID(t *testing.T) {
	jobConfig := []byte(`{"Source":{"CatalogId":"catalog-123"}}`)
	id, ok := pipeline.ExtractCatalogID(jobConfig)
	if !ok || id != "catalog-123" {
		t.Fatalf("got (%q, %v), want (\"catalog-123\", true)", id, ok)
	}
}

func TestCandidateGenerator_ExtractCatalogID_NonCatalogAndMalformed(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{"document_content variant", `{"Source":{"DocumentContent":"abc"}}`},
		{"document_uri variant", `{"Source":{"DocumentUri":"file:///x"}}`},
		{"empty source", `{"Source":{}}`},
		{"no source", `{}`},
		{"empty catalog id", `{"Source":{"CatalogId":""}}`},
		{"malformed json", `{not json`},
		{"empty input", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := pipeline.ExtractCatalogID([]byte(tc.config))
			if ok || id != "" {
				t.Fatalf("got (%q, %v), want (\"\", false)", id, ok)
			}
		})
	}
}

func TestCandidateGenerator_FailsBelowCoverageThreshold(t *testing.T) {
	// 3 controls, only 1 embedded -> 33% coverage < 0.8 default -> error, no candidates written.
	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: threeControls},
		fakeEmbeddingsReader{ids: []string{"c1"}, values: [][]float32{{100}}},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if err == nil {
		t.Fatal("expected coverage error")
	}
	if !errors.Is(err, pipeline.ErrInsufficientEmbeddingCoverage) {
		t.Fatalf("expected ErrInsufficientEmbeddingCoverage, got %v", err)
	}
	if writer.requiresCalled || writer.relationshipCalled {
		t.Fatalf("no candidates should be written below the coverage threshold")
	}
}

func TestCandidateGenerator_ScoreScaling(t *testing.T) {
	// requires.RequiresPair.AggregateScore keeps the candidate's [0,1] score,
	// while relationship.CandidatePair.SimilarityScore is contractually [0,100],
	// so the relationship side must be rescaled by 100.
	registry := candidate.NewRegistry()
	if err := registry.Register(fixedGenerator{
		name: "stub",
		candidates: []candidate.Candidate{
			{SourceID: "c1", TargetID: "c2", Score: 0.8, Weight: 1.0, GeneratorID: "stub"},
		},
	}); err != nil {
		t.Fatalf("register generator: %v", err)
	}

	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: map[string]*candidate.ControlData{
			"c1": {ControlID: "c1"}, "c2": {ControlID: "c2"},
		}},
		fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
		registry, writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	if err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`)); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(writer.requiresPairs) != 1 || len(writer.relationshipPairs) != 1 {
		t.Fatalf("expected 1 pair each, got requires=%d relationship=%d", len(writer.requiresPairs), len(writer.relationshipPairs))
	}
	if got := writer.requiresPairs[0].AggregateScore; got != 0.8 {
		t.Errorf("requires AggregateScore = %v, want 0.8 (unscaled [0,1])", got)
	}
	if got := writer.relationshipPairs[0].SimilarityScore; got != 80.0 {
		t.Errorf("relationship SimilarityScore = %v, want 80.0 ([0,100] scaled)", got)
	}
}

func TestCandidateGenerator_ZeroControlsSkipsCoverageCheck(t *testing.T) {
	// A degenerate catalog with zero controls must not divide by zero nor fail
	// the coverage check; it proceeds and writes an empty candidate set.
	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: map[string]*candidate.ControlData{}},
		fakeEmbeddingsReader{},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if err != nil {
		t.Fatalf("zero-control catalog should not error, got %v", err)
	}
	if !writer.requiresCalled || !writer.relationshipCalled {
		t.Fatalf("writers should still be invoked (with empty sets) for a zero-control catalog")
	}
}

func TestCandidateGenerator_CoverageExactlyAtThresholdSucceeds(t *testing.T) {
	// 4 controls, exactly 2 embedded -> 50% coverage == 0.5 threshold -> must
	// proceed (the boundary is inclusive: `coverage < threshold` fails, not
	// `coverage <= threshold`).
	controls := map[string]*candidate.ControlData{
		"c1": {ControlID: "c1"}, "c2": {ControlID: "c2"},
		"c3": {ControlID: "c3"}, "c4": {ControlID: "c4"},
	}
	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: controls},
		fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.5}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if err != nil {
		t.Fatalf("expected coverage exactly at threshold to succeed, got error: %v", err)
	}
	if !writer.requiresCalled || !writer.relationshipCalled {
		t.Fatalf("writers should be invoked when coverage meets the threshold exactly")
	}
}

var errCandidateBoom = errors.New("boom")

func TestCandidateGenerator_ControlsReaderError(t *testing.T) {
	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{err: errCandidateBoom},
		fakeEmbeddingsReader{},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if !errors.Is(err, errCandidateBoom) {
		t.Fatalf("expected wrapped controls-reader error, got %v", err)
	}
	if writer.requiresCalled || writer.relationshipCalled {
		t.Fatal("no candidates should be written when the controls reader fails")
	}
}

func TestCandidateGenerator_EmbeddingsReaderError(t *testing.T) {
	writer := &fakeCandidateWriter{}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: threeControls},
		fakeEmbeddingsReader{err: errCandidateBoom},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if !errors.Is(err, errCandidateBoom) {
		t.Fatalf("expected wrapped embeddings-reader error, got %v", err)
	}
	if writer.requiresCalled || writer.relationshipCalled {
		t.Fatal("no candidates should be written when the embeddings reader fails")
	}
}

func TestCandidateGenerator_WriteRequiresError(t *testing.T) {
	// Zero-control catalog reaches the writers with empty sets (coverage check
	// is skipped), so WriteRequires is exercised without needing a registry.
	writer := &fakeCandidateWriter{requiresErr: errCandidateBoom}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: map[string]*candidate.ControlData{}},
		fakeEmbeddingsReader{},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if !errors.Is(err, errCandidateBoom) {
		t.Fatalf("expected wrapped WriteRequires error, got %v", err)
	}
	if writer.relationshipCalled {
		t.Fatal("WriteRelationship must not be attempted after WriteRequires fails")
	}
}

func TestCandidateGenerator_WriteRelationshipError(t *testing.T) {
	writer := &fakeCandidateWriter{relationshipErr: errCandidateBoom}
	gen := pipeline.NewCandidateGenerator(
		fakeControlsReader{controls: map[string]*candidate.ControlData{}},
		fakeEmbeddingsReader{},
		candidate.NewRegistry(), writer,
		config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
	)
	err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
	if !errors.Is(err, errCandidateBoom) {
		t.Fatalf("expected wrapped WriteRelationship error, got %v", err)
	}
	if !writer.requiresCalled {
		t.Fatal("WriteRequires should have been called before WriteRelationship")
	}
}
