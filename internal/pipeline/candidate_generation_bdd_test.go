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
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
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

// requestCapturingGenerator records the GenerateRequest it receives.
type requestCapturingGenerator struct {
	got *candidate.GenerateRequest
}

func (g requestCapturingGenerator) Name() string { return "capture" }

func (g requestCapturingGenerator) Generate(_ context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	*g.got = req
	return nil, nil
}

var threeControls = map[string]*candidate.ControlData{
	"c1": {ControlID: "c1", Text: "control one"},
	"c2": {ControlID: "c2", Text: "control two"},
	"c3": {ControlID: "c3", Text: "control three"},
}

var errCandidateBoom = errors.New("boom")

var _ = Describe("CandidateGenerator", func() {
	It("fails a job config without catalog_id", func() {
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{}, fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{}`))
		Expect(err).To(MatchError(pipeline.ErrMissingCatalogID))
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse())
	})

	It("extracts the catalog ID", func() {
		jobConfig := []byte(`{"catalog_id":"catalog-123"}`)
		id, ok := pipeline.ExtractCatalogID(jobConfig)
		Expect(ok).To(BeTrue())
		Expect(id).To(Equal("catalog-123"))
	})

	DescribeTable("ExtractCatalogID on configs without a catalog_id",
		func(jobConfig string) {
			id, ok := pipeline.ExtractCatalogID([]byte(jobConfig))
			Expect(ok).To(BeFalse())
			Expect(id).To(BeEmpty())
		},
		Entry("empty object", `{}`),
		Entry("empty catalog id", `{"catalog_id":""}`),
		Entry("non-string catalog id", `{"catalog_id":7}`),
		Entry("pre-import-only oneof shape", `{"Source":{"CatalogId":"cat-1"}}`),
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
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
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
				"c1": {ControlID: "c1", Text: "one"}, "c2": {ControlID: "c2", Text: "two"},
			}},
			fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
			registry, writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())

		Expect(writer.requiresPairs).To(HaveLen(1))
		Expect(writer.relationshipPairs).To(HaveLen(1))
		Expect(writer.requiresPairs[0].AggregateScore).To(Equal(0.8), "unscaled [0,1]")
		Expect(writer.relationshipPairs[0].SimilarityScore).To(BeNumerically("==", 80.0), "[0,100] scaled")
	})

	It("leaves controls without a statement out of the generator request", func() {
		var got candidate.GenerateRequest
		registry := candidate.NewRegistry()
		Expect(registry.Register(requestCapturingGenerator{got: &got})).To(Succeed())
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{
				"c1": {ControlID: "c1", Text: "one"},
				"c2": {ControlID: "c2", Text: ""},
				"c3": {ControlID: "c3", Text: " \t\n "},
			}},
			fakeEmbeddingsReader{ids: []string{"c1"}, values: [][]float32{{100}}},
			registry, &fakeCandidateWriter{},
			config.CandidateConfig{MinEmbeddingCoverage: 1.0}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())

		Expect(got.SourceControls).To(HaveLen(1))
		Expect(got.SourceControls).To(HaveKey("c1"))
		Expect(got.TargetControls).To(HaveLen(1))
		Expect(got.TargetControls).To(HaveKey("c1"))
	})

	It("ignores controls without a statement when computing coverage", func() {
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{
				"c1": {ControlID: "c1", Text: "one"},
				"c2": {ControlID: "c2", Text: "two"},
				"c3": {ControlID: "c3", Text: ""},
			}},
			fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 1.0}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())
		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue())
	})

	It("records the blank-statement count on the active span when every control is blank", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		ctx, span := tp.TracerProvider().Tracer("test").Start(context.Background(), "job")
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{
				"c1": {ControlID: "c1", Text: ""},
				"c2": {ControlID: "c2", Text: "  "},
			}},
			fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 1.0}, "test-model",
		)
		Expect(gen.Generate(ctx, "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())
		span.End()

		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue())
		recorded := telemetrytest.FindSpan(tp.GetSpans(), "job")
		Expect(recorded).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(recorded, "candidates.skipped_blank_statement")
		Expect(ok).To(BeTrue())
		Expect(v.AsInt64()).To(Equal(int64(2)))
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
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(err).NotTo(HaveOccurred(), "zero-control catalog should not error")
		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue(), "writers should still be invoked (with empty sets) for a zero-control catalog")
	})

	It("succeeds when coverage is exactly at the threshold", func() {
		// 4 controls, exactly 2 embedded -> 50% coverage == 0.5 threshold -> must
		// proceed (the boundary is inclusive: `coverage < threshold` fails, not
		// `coverage <= threshold`).
		controls := map[string]*candidate.ControlData{
			"c1": {ControlID: "c1", Text: "one"}, "c2": {ControlID: "c2", Text: "two"},
			"c3": {ControlID: "c3", Text: "three"}, "c4": {ControlID: "c4", Text: "four"},
		}
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: controls},
			fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.5}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(err).NotTo(HaveOccurred(), "expected coverage exactly at threshold to succeed")
		Expect(writer.requiresCalled && writer.relationshipCalled).To(BeTrue(), "writers should be invoked when coverage meets the threshold exactly")
	})

	DescribeTable("wraps a reader error and writes nothing",
		func(controls pipeline.ControlsReader, embeddings pipeline.EmbeddingsReader) {
			writer := &fakeCandidateWriter{}
			gen := pipeline.NewCandidateGenerator(
				controls, embeddings,
				candidate.NewRegistry(), writer,
				config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
			)
			err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
			Expect(err).To(MatchError(errCandidateBoom))
			Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse(), "no candidates should be written when a reader fails")
		},
		Entry("the controls reader fails",
			fakeControlsReader{err: errCandidateBoom}, fakeEmbeddingsReader{}),
		Entry("the embeddings reader fails",
			fakeControlsReader{controls: threeControls}, fakeEmbeddingsReader{err: errCandidateBoom}),
	)

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
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
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
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(errors.Is(err, errCandidateBoom)).To(BeTrue(), "expected wrapped WriteRelationship error, got %v", err)
		Expect(writer.requiresCalled).To(BeTrue(), "WriteRequires should have been called before WriteRelationship")
	})
})
