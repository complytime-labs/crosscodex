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
