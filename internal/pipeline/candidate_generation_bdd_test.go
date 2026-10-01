package pipeline_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

var errCandidateBoom = errors.New("boom")

var _ = Describe("CandidateGenerator", func() {
	It("skips non-catalog jobs", func() {
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{}, fakeEmbeddingsReader{},
			candidate.NewRegistry(), &fakeCandidateWriter{},
			config.CandidateConfig{}, "test-model",
		)
		jobConfig := []byte(`{"Source":{"DocumentContent":"..."}}`)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", jobConfig)
		Expect(errors.Is(err, pipeline.ErrNotACatalogJob)).To(BeTrue(), "expected ErrNotACatalogJob, got %v", err)
	})

	It("extracts the catalog ID", func() {
		jobConfig := []byte(`{"Source":{"CatalogId":"catalog-123"}}`)
		id, ok := pipeline.ExtractCatalogID(jobConfig)
		Expect(ok).To(BeTrue())
		Expect(id).To(Equal("catalog-123"))
	})

	DescribeTable("ExtractCatalogID on non-catalog and malformed configs",
		func(jobConfig string) {
			id, ok := pipeline.ExtractCatalogID([]byte(jobConfig))
			Expect(ok).To(BeFalse())
			Expect(id).To(BeEmpty())
		},
		Entry("document_content variant", `{"Source":{"DocumentContent":"abc"}}`),
		Entry("document_uri variant", `{"Source":{"DocumentUri":"file:///x"}}`),
		Entry("empty source", `{"Source":{}}`),
		Entry("no source", `{}`),
		Entry("empty catalog id", `{"Source":{"CatalogId":""}}`),
		Entry("malformed json", `{not json`),
		Entry("empty input", ``),
	)

	It("fails below the coverage threshold", func() {
		// 3 controls, only 1 embedded -> 33% coverage < 0.8 default -> error, no candidates written.
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: threeControls},
			fakeEmbeddingsReader{ids: []string{"c1"}, values: [][]float32{{100}}},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, pipeline.ErrInsufficientEmbeddingCoverage)).To(BeTrue(), "expected ErrInsufficientEmbeddingCoverage, got %v", err)
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse(), "no candidates should be written below the coverage threshold")
	})

	It("scales the relationship score to [0,100] while leaving the requires score at [0,1]", func() {
		// requires.RequiresPair.AggregateScore keeps the candidate's [0,1] score,
		// while relationship.CandidatePair.SimilarityScore is contractually [0,100],
		// so the relationship side must be rescaled by 100.
		registry := candidate.NewRegistry()
		Expect(registry.Register(fixedGenerator{
			name: "stub",
			candidates: []candidate.Candidate{
				{SourceID: "c1", TargetID: "c2", Score: 0.8, Weight: 1.0, GeneratorID: "stub"},
			},
		})).To(Succeed())

		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{
				"c1": {ControlID: "c1"}, "c2": {ControlID: "c2"},
			}},
			fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
			registry, writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))).To(Succeed())

		Expect(writer.requiresPairs).To(HaveLen(1))
		Expect(writer.relationshipPairs).To(HaveLen(1))
		Expect(writer.requiresPairs[0].AggregateScore).To(Equal(0.8), "unscaled [0,1]")
		Expect(writer.relationshipPairs[0].SimilarityScore).To(BeNumerically("==", 80.0), "[0,100] scaled")
	})

	It("skips the coverage check for a zero-control catalog", func() {
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
		Expect(err).NotTo(HaveOccurred(), "zero-control catalog should not error")
		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue(), "writers should still be invoked (with empty sets) for a zero-control catalog")
	})

	It("succeeds when coverage is exactly at the threshold", func() {
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
		Expect(err).NotTo(HaveOccurred(), "expected coverage exactly at threshold to succeed")
		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue(), "writers should be invoked when coverage meets the threshold exactly")
	})

	It("wraps a controls-reader error and writes nothing", func() {
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{err: errCandidateBoom},
			fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
		Expect(errors.Is(err, errCandidateBoom)).To(BeTrue(), "expected wrapped controls-reader error, got %v", err)
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse(), "no candidates should be written when the controls reader fails")
	})

	It("wraps an embeddings-reader error and writes nothing", func() {
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: threeControls},
			fakeEmbeddingsReader{err: errCandidateBoom},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
		Expect(errors.Is(err, errCandidateBoom)).To(BeTrue(), "expected wrapped embeddings-reader error, got %v", err)
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse(), "no candidates should be written when the embeddings reader fails")
	})

	It("stops at WriteRequires error without attempting WriteRelationship", func() {
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
		Expect(errors.Is(err, errCandidateBoom)).To(BeTrue(), "expected wrapped WriteRequires error, got %v", err)
		Expect(writer.relationshipCalled).To(BeFalse(), "WriteRelationship must not be attempted after WriteRequires fails")
	})

	It("wraps a WriteRelationship error after calling WriteRequires", func() {
		writer := &fakeCandidateWriter{relationshipErr: errCandidateBoom}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{}},
			fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"Source":{"CatalogId":"cat-1"}}`))
		Expect(errors.Is(err, errCandidateBoom)).To(BeTrue(), "expected wrapped WriteRelationship error, got %v", err)
		Expect(writer.requiresCalled).To(BeTrue(), "WriteRequires should have been called before WriteRelationship")
	})
})
