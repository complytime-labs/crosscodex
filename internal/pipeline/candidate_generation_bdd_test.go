package pipeline_test

import (
	"context"
	"errors"
	"strconv"

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

// fakeEmbeddingsReader is a narrow EmbeddingsReader test double. When
// gotCatalogIDs is set it records the catalog list it was asked for.
type fakeEmbeddingsReader struct {
	ids           []string
	values        [][]float32
	err           error
	gotCatalogIDs *[]string
}

func (f fakeEmbeddingsReader) SimilarityMatrix(_ context.Context, _ string, catalogIDs []string, _ string) ([]string, [][]float32, error) {
	if f.gotCatalogIDs != nil {
		*f.gotCatalogIDs = catalogIDs
	}
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

// catalogControls is a ControlsReader serving each catalog's controls.
type catalogControls map[string]map[string]*candidate.ControlData

func (f catalogControls) ControlData(_ context.Context, _, catalogID, _ string) (map[string]*candidate.ControlData, error) {
	return f[catalogID], nil
}

// allPairsGenerator proposes every source-target pair of distinct
// controls, so a spec sees exactly which pairs the request allowed.
type allPairsGenerator struct{}

func (allPairsGenerator) Name() string { return "all-pairs" }

func (allPairsGenerator) Generate(_ context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	var out []candidate.Candidate
	for s := range req.SourceControls {
		for t := range req.TargetControls {
			if s != t {
				out = append(out, candidate.Candidate{SourceID: s, TargetID: t, Score: 1, Weight: 1, GeneratorID: "all-pairs"})
			}
		}
	}
	return out, nil
}

// crosswalkControls is a source catalog of two controls and a target
// catalog of two, every control embedded.
var crosswalkControls = catalogControls{
	"cat-src": {"s1": {ControlID: "s1", Text: "source one"}, "s2": {ControlID: "s2", Text: "source two"}},
	"cat-tgt": {"t1": {ControlID: "t1", Text: "target one"}, "t2": {ControlID: "t2", Text: "target two"}},
}

// writtenPairs returns the (source, target) pairs the writer received.
func writtenPairs(w *fakeCandidateWriter) [][2]string {
	pairs := make([][2]string, len(w.relationshipPairs))
	for i, p := range w.relationshipPairs {
		pairs[i] = [2]string{p.SourceControlID, p.TargetControlID}
	}
	return pairs
}

var threeControls = map[string]*candidate.ControlData{
	"c1": {ControlID: "c1", Text: "control one"},
	"c2": {ControlID: "c2", Text: "control two"},
	"c3": {ControlID: "c3", Text: "control three"},
}

var errCandidateBoom = errors.New("boom")

var _ = Describe("CandidateGenerator", func() {
	Describe("across two catalogs", func() {
		var (
			writer        *fakeCandidateWriter
			gotCatalogIDs []string
		)

		generate := func(jobConfig string, controls catalogControls, embeddedIDs ...string) error {
			registry := candidate.NewRegistry()
			Expect(registry.Register(allPairsGenerator{})).To(Succeed())
			writer = &fakeCandidateWriter{}
			values := make([][]float32, len(embeddedIDs))
			for i := range values {
				values[i] = make([]float32, len(embeddedIDs))
			}
			gen := pipeline.NewCandidateGenerator(
				controls,
				fakeEmbeddingsReader{ids: embeddedIDs, values: values, gotCatalogIDs: &gotCatalogIDs},
				registry, writer,
				config.CandidateConfig{MinEmbeddingCoverage: 1.0, MaxPerSource: 10}, "test-model",
			)
			return gen.Generate(context.Background(), "tenant-1", "job-1", []byte(jobConfig))
		}

		It("pairs only source controls with target controls", func() {
			Expect(generate(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt"}`, crosswalkControls, "s1", "s2", "t1", "t2")).To(Succeed())
			Expect(writtenPairs(writer)).To(ConsistOf(
				[2]string{"s1", "t1"}, [2]string{"s1", "t2"}, [2]string{"s2", "t1"}, [2]string{"s2", "t2"},
			))
			Expect(writer.requiresPairs).To(HaveLen(4))
		})

		It("adds target-to-source pairs with bidirectional, and still no pair within one catalog", func() {
			Expect(generate(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt","bidirectional":true}`, crosswalkControls, "s1", "s2", "t1", "t2")).To(Succeed())
			Expect(writtenPairs(writer)).To(ConsistOf(
				[2]string{"s1", "t1"}, [2]string{"s1", "t2"}, [2]string{"s2", "t1"}, [2]string{"s2", "t2"},
				[2]string{"t1", "s1"}, [2]string{"t1", "s2"}, [2]string{"t2", "s1"}, [2]string{"t2", "s2"},
			))
		})

		It("reads one similarity matrix over both catalogs, source first", func() {
			Expect(generate(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt"}`, crosswalkControls, "s1", "s2", "t1", "t2")).To(Succeed())
			Expect(gotCatalogIDs).To(Equal([]string{"cat-src", "cat-tgt"}))
		})

		DescribeTable("fails when one side's embedding coverage falls short, naming that catalog, and writes nothing",
			func(embeddedIDs []string, want string) {
				err := generate(`{"catalog_id":"cat-src","target_catalog_id":"cat-tgt"}`, crosswalkControls, embeddedIDs...)
				Expect(err).To(MatchError(pipeline.ErrInsufficientEmbeddingCoverage))
				Expect(err.Error()).To(ContainSubstring(want))
				Expect(err.Error()).To(ContainSubstring("analysis.candidates.min_embedding_coverage"))
				Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse())
			},
			Entry("the target", []string{"s1", "s2", "t1"}, "target catalog cat-tgt: 1 of 2 controls"),
			Entry("the source", []string{"s1", "t1", "t2"}, "source catalog cat-src: 1 of 2 controls"),
		)

		It("pairs a catalog's controls with each other in both directions within one catalog", func() {
			Expect(generate(`{"catalog_id":"cat-src","target_catalog_id":"cat-src"}`, crosswalkControls, "s1", "s2")).To(Succeed())
			Expect(writtenPairs(writer)).To(ConsistOf([2]string{"s1", "s2"}, [2]string{"s2", "s1"}))
			Expect(gotCatalogIDs).To(Equal([]string{"cat-src"}))
		})
	})

	It("reads the similarity matrix of the job's one catalog within one catalog", func() {
		var got []string
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: threeControls},
			fakeEmbeddingsReader{ids: []string{"c1", "c2", "c3"}, values: [][]float32{{100, 0, 0}, {0, 100, 0}, {0, 0, 100}}, gotCatalogIDs: &got},
			candidate.NewRegistry(), &fakeCandidateWriter{},
			config.CandidateConfig{MinEmbeddingCoverage: 1.0, MaxPerSource: 10}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1","target_catalog_id":"cat-1"}`))).To(Succeed())
		Expect(got).To(Equal([]string{"cat-1"}))
	})

	It("fails a job config without catalog_id", func() {
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{}, fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MaxPerSource: 10}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{}`))
		Expect(err).To(MatchError(pipeline.ErrMissingCatalogID))
		Expect(err.Error()).To(ContainSubstring("start a new analysis: crosscodex run start --source <source-catalog-id> --target <target-catalog-id>"))
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse())
	})

	It("fails below the coverage threshold", func() {
		// 3 controls, only 1 embedded -> 33% coverage < 0.8 default -> error, no candidates written.
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: threeControls},
			fakeEmbeddingsReader{ids: []string{"c1"}, values: [][]float32{{100}}},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(err).To(MatchError(pipeline.ErrInsufficientEmbeddingCoverage))
		Expect(writer.requiresCalled || writer.relationshipCalled).To(BeFalse(), "no candidates should be written below the coverage threshold")
	})

	It("sets the relationship score to the pair's similarity-matrix value and the requires score to the fused value", func() {
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
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())

		Expect(writer.requiresPairs).To(HaveLen(1))
		Expect(writer.relationshipPairs).To(HaveLen(1))
		Expect(writer.requiresPairs[0].AggregateScore).To(BeNumerically("==", 1.0/61), "fused reciprocal-rank value")
		Expect(writer.relationshipPairs[0].SimilarityScore).To(BeNumerically("==", 80.0), "matrix value")
	})

	Describe("with semantic and lexical generators", func() {
		lexicalControls := map[string]*candidate.ControlData{
			"c1": {ControlID: "c1", Text: "encrypt stored data"},
			"c2": {ControlID: "c2", Text: "encrypt data in transit"},
			"c3": {ControlID: "c3", Text: "rotate widgets monthly"},
		}

		generate := func(embeddedIDs []string, values [][]float32) *fakeCandidateWriter {
			cfg := config.CandidateConfig{
				MinEmbeddingCoverage: 0,
				MaxPerSource:         10,
				Generators: []config.CandidateGeneratorEntry{
					{Name: "semantic", Enabled: true},
					{Name: "lexical", Enabled: true},
				},
			}
			reg, err := pipeline.BuildCandidateRegistry(cfg, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			writer := &fakeCandidateWriter{}
			gen := pipeline.NewCandidateGenerator(
				fakeControlsReader{controls: lexicalControls},
				fakeEmbeddingsReader{ids: embeddedIDs, values: values},
				reg, writer, cfg, "test-model",
			)
			Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())
			return writer
		}

		findRequires := func(w *fakeCandidateWriter, source, target string) requires.RequiresPair {
			for _, p := range w.requiresPairs {
				if p.SourceControlID == source && p.TargetControlID == target {
					return p
				}
			}
			Fail("no requires pair " + source + "--" + target)
			return requires.RequiresPair{}
		}

		It("records one ranked provenance entry per contributing generator and the fused score", func() {
			w := generate([]string{"c1", "c2", "c3"}, [][]float32{{100, 90, 10}, {90, 100, 20}, {10, 20, 100}})

			pair := findRequires(w, "c1", "c2")
			Expect(pair.Provenance).To(HaveLen(2))
			Expect([]string{pair.Provenance[0].GeneratorName, pair.Provenance[1].GeneratorName}).To(Equal([]string{"lexical", "semantic"}))
			fused := 0.0
			for _, prov := range pair.Provenance {
				Expect(prov.Metadata).To(HaveKey("rank"))
				rank, err := strconv.ParseFloat(prov.Metadata["rank"], 64)
				Expect(err).NotTo(HaveOccurred())
				fused += prov.Weight / (60 + rank)
			}
			Expect(pair.AggregateScore).To(BeNumerically("~", fused, 1e-12))
		})

		It("reports the matrix similarity, and 0 for a lexical-only pair whose target has no matrix row", func() {
			w := generate([]string{"c1", "c3"}, [][]float32{{100, 10}, {10, 100}})

			sims := map[string]float32{}
			for _, p := range w.relationshipPairs {
				sims[p.SourceControlID+"--"+p.TargetControlID] = p.SimilarityScore
			}
			Expect(sims).To(HaveKeyWithValue("c1--c3", float32(10)))
			Expect(sims).To(HaveKeyWithValue("c1--c2", float32(0)), "c2 has no embedding row")
		})
	})

	It("does not write the rank into a generator's own metadata map", func() {
		shared := map[string]string{"method": "stub"}
		registry := candidate.NewRegistry()
		Expect(registry.Register(fixedGenerator{
			name: "stub",
			candidates: []candidate.Candidate{
				{SourceID: "c1", TargetID: "c2", Score: 0.8, Weight: 1.0, GeneratorID: "stub", Metadata: shared},
				{SourceID: "c2", TargetID: "c1", Score: 0.7, Weight: 1.0, GeneratorID: "stub", Metadata: shared},
			},
		})).To(Succeed())
		writer := &fakeCandidateWriter{}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{
				"c1": {ControlID: "c1", Text: "one"}, "c2": {ControlID: "c2", Text: "two"},
			}},
			fakeEmbeddingsReader{ids: []string{"c1", "c2"}, values: [][]float32{{100, 80}, {80, 100}}},
			registry, writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
		)
		Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())

		Expect(shared).To(Equal(map[string]string{"method": "stub"}))
		Expect(writer.requiresPairs).To(HaveLen(2))
		for _, p := range writer.requiresPairs {
			Expect(p.Provenance[0].Metadata).To(HaveKeyWithValue("method", "stub"))
			Expect(p.Provenance[0].Metadata).To(HaveKey("rank"))
		}
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
			config.CandidateConfig{MinEmbeddingCoverage: 1.0, MaxPerSource: 10}, "test-model",
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
			config.CandidateConfig{MinEmbeddingCoverage: 1.0, MaxPerSource: 10}, "test-model",
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
			config.CandidateConfig{MinEmbeddingCoverage: 1.0, MaxPerSource: 10}, "test-model",
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
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
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
			config.CandidateConfig{MinEmbeddingCoverage: 0.5, MaxPerSource: 10}, "test-model",
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
				config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
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
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(err).To(MatchError(errCandidateBoom))
		Expect(writer.relationshipCalled).To(BeFalse(), "WriteRelationship must not be attempted after WriteRequires fails")
	})

	It("wraps a WriteRelationship error after calling WriteRequires", func() {
		writer := &fakeCandidateWriter{relationshipErr: errCandidateBoom}
		gen := pipeline.NewCandidateGenerator(
			fakeControlsReader{controls: map[string]*candidate.ControlData{}},
			fakeEmbeddingsReader{},
			candidate.NewRegistry(), writer,
			config.CandidateConfig{MinEmbeddingCoverage: 0.8, MaxPerSource: 10}, "test-model",
		)
		err := gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))
		Expect(err).To(MatchError(errCandidateBoom))
		Expect(writer.requiresCalled).To(BeTrue(), "WriteRequires should have been called before WriteRelationship")
	})
})
