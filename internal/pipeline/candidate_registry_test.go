package pipeline_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

var _ = Describe("BuildCandidateRegistry", func() {
	It("registers enabled builtin generators and skips disabled ones", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "keyword", Enabled: true},
			{Name: "level", Enabled: true},
			{Name: "semantic", Enabled: false},
		}}

		reg, err := pipeline.BuildCandidateRegistry(cfg, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("keyword")
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("level")
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("semantic")
		Expect(err).To(HaveOccurred())
	})

	It("registers the semantic generator when enabled", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "semantic", Enabled: true},
		}}

		reg, err := pipeline.BuildCandidateRegistry(cfg, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("semantic")
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects unknown generator names loudly instead of skipping them", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "typo-generator", Enabled: true},
		}}

		_, err := pipeline.BuildCandidateRegistry(cfg, nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("typo-generator"))
	})

	It("returns an empty registry when no generators are configured", func() {
		cfg := config.CandidateConfig{}

		reg, err := pipeline.BuildCandidateRegistry(cfg, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(reg.All()).To(BeEmpty())
	})

	Describe("per-generator weight and config", func() {
		controls := map[string]*candidate.ControlData{
			"c1": {ControlID: "c1", Text: "Establish a policy", Level: "Strategic"},
			"c2": {ControlID: "c2", Text: "alpha review", Level: "Operational"},
			"c3": {ControlID: "c3", Text: "rotate keys", Level: "Operational"},
		}
		ids := []string{"c1", "c2", "c3"}
		values := [][]float32{{100, 80, 60}, {80, 100, 70}, {60, 70, 100}}
		// request mirrors what CandidateGenerator.Generate sends: no Parameters,
		// so anything a generator sees there came from its config entry.
		request := candidate.GenerateRequest{
			TenantID: "tenant-1", JobID: "job-1",
			SourceControls: controls, TargetControls: controls,
			EmbeddingMatrix: struct {
				IDs    []string
				Values [][]float32
			}{ids, values},
		}
		generate := func(reg *candidate.Registry, name string) []candidate.Candidate {
			gen, err := reg.Get(name)
			Expect(err).NotTo(HaveOccurred())
			candidates, err := gen.Generate(context.Background(), request)
			Expect(err).NotTo(HaveOccurred())
			return candidates
		}

		It("passes each generator only its own weight and config", func() {
			reg, err := pipeline.BuildCandidateRegistry(config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
				{Name: "semantic", Enabled: true, Weight: 0.4, Config: map[string]interface{}{"top_k": 1}},
				{Name: "keyword", Enabled: true, Weight: 0.7, Config: map[string]interface{}{"keywords": []interface{}{"alpha"}}},
			}}, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			semantic := generate(reg, "semantic")
			Expect(semantic).To(HaveLen(3), "top_k 1 yields one candidate per source")
			for _, c := range semantic {
				Expect(c.Weight).To(Equal(0.4))
			}

			keyword := generate(reg, "keyword")
			Expect(keyword).To(HaveLen(2), "only c2 contains the configured keyword; c2 is never its own candidate")
			for _, c := range keyword {
				Expect(c.TargetID).To(Equal("c2"))
				Expect(c.Weight).To(Equal(0.7))
			}
		})

		DescribeTable("hands the generator its entry's weight",
			func(weight, want float64) {
				reg, err := pipeline.BuildCandidateRegistry(config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
					{Name: "level", Enabled: true, Weight: weight},
				}}, nil, nil)
				Expect(err).NotTo(HaveOccurred())

				candidates := generate(reg, "level")
				Expect(candidates).NotTo(BeEmpty())
				for _, c := range candidates {
					Expect(c.Weight).To(Equal(want))
				}
			},
			Entry("a configured weight", 0.25, 0.25),
			Entry("0 as the default 1.0", 0.0, 1.0),
		)

		It("leaves the config entry's map unmodified", func() {
			entryConfig := map[string]interface{}{"top_k": 1}
			_, err := pipeline.BuildCandidateRegistry(config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
				{Name: "semantic", Enabled: true, Weight: 0.4, Config: entryConfig},
			}}, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(entryConfig).To(Equal(map[string]interface{}{"top_k": 1}))
		})

		It("applies semantic top_k end to end through CandidateGenerator.Generate", func() {
			reg, err := pipeline.BuildCandidateRegistry(config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
				{Name: "semantic", Enabled: true, Weight: 0.4, Config: map[string]interface{}{"top_k": 1}},
			}}, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			writer := &fakeCandidateWriter{}
			gen := pipeline.NewCandidateGenerator(
				fakeControlsReader{controls: controls},
				fakeEmbeddingsReader{ids: ids, values: values},
				reg, writer,
				config.CandidateConfig{MinEmbeddingCoverage: 0.8}, "test-model",
			)

			Expect(gen.Generate(context.Background(), "tenant-1", "job-1", []byte(`{"catalog_id":"cat-1"}`))).To(Succeed())
			Expect(writer.requiresPairs).To(HaveLen(3), "one pair per source, not the default top_k of 10")
			for _, p := range writer.requiresPairs {
				Expect(p.Provenance).To(HaveLen(1))
				Expect(p.Provenance[0].Weight).To(Equal(0.4))
			}
		})
	})

	Describe("telemetry forwarding", func() {
		allGeneratorsConfig := func() config.CandidateConfig {
			return config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
				{Name: "keyword", Enabled: true},
				{Name: "level", Enabled: true},
				{Name: "semantic", Enabled: true},
			}}
		}

		It("records a registration for each enabled generator on the registry's own counter", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())

			_, err = pipeline.BuildCandidateRegistry(allGeneratorsConfig(), tp.TracerProvider(), tp.MeterProvider())
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "candidate.registrations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected candidate.registrations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))
		})

		DescribeTable("forwards the tracer provider into each enabled builtin generator",
			func(name string) {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(tp.Shutdown, context.Background())

				reg, err := pipeline.BuildCandidateRegistry(allGeneratorsConfig(), tp.TracerProvider(), tp.MeterProvider())
				Expect(err).NotTo(HaveOccurred())

				_, err = reg.Generate(context.Background(), candidate.GenerateRequest{}, candidate.StrategyUnion)
				Expect(err).NotTo(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, name+".Generate")
				Expect(span).NotTo(BeNil(), "expected a %s.Generate span", name)
				Expect(span.InstrumentationScope().Name).To(Equal("crosscodex/pkg/analyzer/candidate/builtin"))
			},
			Entry("keyword", "keyword"),
			Entry("level", "level"),
			Entry("semantic", "semantic"),
		)
	})
})
