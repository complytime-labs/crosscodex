package classify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestClassifyBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Classify Analyzer BDD Suite")
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

var _ = Describe("ClassificationType", func() {
	DescribeTable("String returns canonical form",
		func(t classify.ClassificationType, expected string) {
			Expect(t.String()).To(Equal(expected))
		},
		Entry("None", classify.TypeNone, "None"),
		Entry("Technical", classify.TypeTechnical, "Technical"),
		Entry("Procedural", classify.TypeProcedural, "Procedural"),
		Entry("Both", classify.TypeBoth, "Both"),
	)

	DescribeTable("Valid reports defined values",
		func(t classify.ClassificationType, expected bool) {
			Expect(t.Valid()).To(Equal(expected))
		},
		Entry("None is valid", classify.TypeNone, true),
		Entry("Technical is valid", classify.TypeTechnical, true),
		Entry("Both is valid", classify.TypeBoth, true),
		Entry("out of range is invalid", classify.ClassificationType(99), false),
	)
})

var _ = Describe("ClassificationLevel", func() {
	DescribeTable("String returns canonical form",
		func(l classify.ClassificationLevel, expected string) {
			Expect(l.String()).To(Equal(expected))
		},
		Entry("None", classify.LevelNone, "None"),
		Entry("Strategic", classify.LevelStrategic, "Strategic"),
		Entry("Tactical", classify.LevelTactical, "Tactical"),
		Entry("Operational", classify.LevelOperational, "Operational"),
	)

	DescribeTable("Valid reports defined values",
		func(l classify.ClassificationLevel, expected bool) {
			Expect(l.Valid()).To(Equal(expected))
		},
		Entry("None is valid", classify.LevelNone, true),
		Entry("Operational is valid", classify.LevelOperational, true),
		Entry("out of range is invalid", classify.ClassificationLevel(99), false),
	)
})

var _ = Describe("AllTypes", func() {
	It("returns all four types in declaration order", func() {
		types := classify.AllTypes()
		Expect(types).To(Equal([]classify.ClassificationType{
			classify.TypeNone, classify.TypeTechnical, classify.TypeProcedural, classify.TypeBoth,
		}))
	})
})

var _ = Describe("AllLevels", func() {
	It("returns all four levels in declaration order", func() {
		levels := classify.AllLevels()
		Expect(levels).To(Equal([]classify.ClassificationLevel{
			classify.LevelNone, classify.LevelStrategic, classify.LevelTactical, classify.LevelOperational,
		}))
	})
})

var _ = Describe("ValidCombinations", func() {
	It("returns exactly 10 legal pairs", func() {
		combos := classify.ValidCombinations()
		// 1 (None|None) + 3 types * 3 levels = 10
		Expect(combos).To(HaveLen(10))
	})

	It("includes None|None exactly once", func() {
		combos := classify.ValidCombinations()
		noneCount := 0
		for _, c := range combos {
			if c.Type == classify.TypeNone && c.Level == classify.LevelNone {
				noneCount++
			}
		}
		Expect(noneCount).To(Equal(1))
	})

	It("excludes None type with non-None levels", func() {
		combos := classify.ValidCombinations()
		for _, c := range combos {
			if c.Type == classify.TypeNone {
				Expect(c.Level).To(Equal(classify.LevelNone))
			}
		}
	})

	It("excludes LevelNone for non-None types", func() {
		combos := classify.ValidCombinations()
		for _, c := range combos {
			if c.Type != classify.TypeNone {
				Expect(c.Level).NotTo(Equal(classify.LevelNone))
			}
		}
	})

	It("every combination roundtrips through String/Parse", func() {
		for _, c := range classify.ValidCombinations() {
			parsed, err := classify.ParseClassification(c.String())
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed.Type).To(Equal(c.Type))
			Expect(parsed.Level).To(Equal(c.Level))
		}
	})
})

var _ = Describe("Result", func() {
	It("formats as Type|Level", func() {
		r := classify.Result{
			ControlID: "AC-1",
			Type:      classify.TypeTechnical,
			Level:     classify.LevelOperational,
		}
		Expect(r.String()).To(Equal("Technical|Operational"))
	})

	It("formats None|None", func() {
		r := classify.Result{Type: classify.TypeNone, Level: classify.LevelNone}
		Expect(r.String()).To(Equal("None|None"))
	})
})

// --- Mock LLM client for analyzer tests ---

type mockLLMClient struct {
	completeFunc func(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error)
}

func (m *mockLLMClient) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	if m.completeFunc != nil {
		return m.completeFunc(ctx, req)
	}
	return &llmclient.CompletionResponse{
		Choices: []llmclient.CompletionChoice{{
			Message: llmclient.ChatMessage{Role: "assistant", Content: "Technical|Operational"},
		}},
	}, nil
}

func (m *mockLLMClient) Embed(_ context.Context, _ *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockLLMClient) Health(_ context.Context) error { return nil }
func (m *mockLLMClient) Close() error                   { return nil }

// --- ClassifyAnalyzer tests ---

var _ = Describe("ClassifyAnalyzer", func() {
	var (
		a          *classify.ClassifyAnalyzer
		mockClient *mockLLMClient
		prompts    prompt.Registry
		cfg        config.ClassificationConfig
		ctx        context.Context
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext("test-tenant")

		mockClient = &mockLLMClient{}

		var err error
		prompts, err = prompt.NewRegistry(config.PromptConfig{
			Layers: config.PromptLayerConfig{Enabled: true},
		})
		Expect(err).NotTo(HaveOccurred())

		cfg = config.ClassificationConfig{
			Enabled:       true,
			MaxTextLength: 2000,
			Temperature:   0.0,
			MaxTokens:     20,
		}

		a = classify.New(mockClient, prompts, cfg)
	})

	Describe("Name", func() {
		It("returns 'classify'", func() {
			Expect(a.Name()).To(Equal("classify"))
		})
	})

	Describe("DependsOn", func() {
		It("returns nil (no dependencies)", func() {
			Expect(a.DependsOn()).To(BeNil())
		})
	})

	Describe("ResultSchema", func() {
		It("returns *AnalysisResult", func() {
			schema := a.ResultSchema()
			Expect(schema).NotTo(BeNil())
			_, ok := schema.(*pb.AnalysisResult)
			Expect(ok).To(BeTrue())
		})
	})

	Describe("GenerateWork", func() {
		Context("with a compliance requirement", func() {
			It("produces one task", func() {
				control := &pb.Control{
					ControlId:  "nist-800-53/AC-1",
					Identifier: "AC-1",
					Statement:  "The organization shall define access control policy.",
					Parts:      map[string]string{"class": "compliance-requirement"},
				}
				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
				Expect(tasks[0].TaskType).To(Equal("classify"))
				Expect(tasks[0].TaskID).NotTo(BeEmpty())
			})
		})

		Context("with a compliance section", func() {
			It("produces a pre-classified task with None|None", func() {
				control := &pb.Control{
					ControlId:  "nist-800-53/AC",
					Identifier: "AC",
					Statement:  "ACCESS CONTROL",
					Parts:      map[string]string{"class": "compliance-section"},
				}
				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))

				result, ok := tasks[0].Payload.(*pb.AnalysisResult)
				Expect(ok).To(BeTrue())
				Expect(result.Attributes["type"]).To(Equal("None"))
				Expect(result.Attributes["level"]).To(Equal("None"))
				Expect(result.Attributes["skipped"]).To(Equal("true"))
			})
		})

		Context("text sanitization", func() {
			It("replaces newlines with spaces in payload", func() {
				control := &pb.Control{
					ControlId:  "nist-800-53/AC-1",
					Identifier: "AC-1",
					Statement:  "Line one.\nLine two.\nLine three.",
					Parts:      map[string]string{"class": "compliance-requirement"},
				}

				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
				// Just verify it succeeds -- sanitization is tested via exported helper
			})

			It("truncates text to MaxTextLength", func() {
				longText := strings.Repeat("a", 3000)
				control := &pb.Control{
					ControlId:  "nist-800-53/AC-1",
					Identifier: "AC-1",
					Statement:  longText,
					Parts:      map[string]string{"class": "compliance-requirement"},
				}

				tasks, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tasks).To(HaveLen(1))
				// Verify via exported sanitizeText helper
			})
		})

		Context("without tenant in context", func() {
			It("returns an error", func() {
				control := &pb.Control{
					ControlId: "AC-1",
					Statement: "test",
					Parts:     map[string]string{"class": "compliance-requirement"},
				}
				_, err := a.GenerateWork(context.Background(), control, analyzer.AnalyzerConfig{})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("tenant"))
			})
		})
	})

	Describe("Aggregate", func() {
		It("collects results and counts classified vs skipped", func() {
			results := []analyzer.TaskResult{
				{
					TaskID:   "task-1",
					TaskType: "classify",
					Result: &pb.AnalysisResult{
						ResultType: "classification",
						Attributes: map[string]string{
							"control_id": "AC-1",
							"type":       "Technical",
							"level":      "Operational",
						},
						Confidence: 1.0,
					},
				},
				{
					TaskID:   "task-2",
					TaskType: "classify",
					Result: &pb.AnalysisResult{
						ResultType: "classification",
						Attributes: map[string]string{
							"control_id": "AC",
							"type":       "None",
							"level":      "None",
							"skipped":    "true",
						},
						Confidence: 1.0,
					},
				},
			}

			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("classify"))
			Expect(output.Metadata["classified_count"]).To(Equal("1"))
			Expect(output.Metadata["skipped_count"]).To(Equal("1"))
			Expect(output.Metadata["total_count"]).To(Equal("2"))
		})

		It("counts errors", func() {
			results := []analyzer.TaskResult{
				{
					TaskID:   "task-1",
					TaskType: "classify",
					Error:    errors.New("LLM timeout"),
				},
			}

			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.Metadata["error_count"]).To(Equal("1"))
		})

		It("counts type assertion failures as errors", func() {
			results := []analyzer.TaskResult{
				{
					TaskID:   "task-1",
					TaskType: "classify",
					Result:   &structpb.Struct{}, // Not *pb.AnalysisResult
				},
			}
			output, err := a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.Metadata["error_count"]).To(Equal("1"))
			Expect(output.Metadata["classified_count"]).To(Equal("0"))
			Expect(output.Metadata["skipped_count"]).To(Equal("0"))
		})
	})

	Describe("exported helpers", func() {
		Context("sanitizeText", func() {
			It("replaces newlines with spaces", func() {
				result := classify.ExportSanitizeText("line1\nline2\rline3", 100)
				Expect(result).To(Equal("line1 line2 line3"))
			})

			It("truncates to max length", func() {
				result := classify.ExportSanitizeText(strings.Repeat("a", 100), 50)
				Expect(result).To(HaveLen(50))
			})

			It("truncates multi-byte characters on rune boundaries", func() {
				// 10 CJK characters, each 3 bytes in UTF-8 = 30 bytes total.
				input := strings.Repeat("\u4e16", 10) // "世" repeated
				result := classify.ExportSanitizeText(input, 5)
				Expect([]rune(result)).To(HaveLen(5))
				Expect(result).To(Equal(strings.Repeat("\u4e16", 5)))
			})

			It("handles zero max length by not truncating", func() {
				result := classify.ExportSanitizeText("hello", 0)
				Expect(result).To(Equal("hello"))
			})
		})

		Context("formatFewShotExamples", func() {
			It("formats examples as inline text", func() {
				examples := []prompt.FewShotExample{
					{Input: "Test requirement", Output: "Technical|Operational"},
					{Input: "Another one", Output: "Procedural|Strategic"},
				}
				result := classify.ExportFormatFewShotExamples(examples)
				Expect(result).To(ContainSubstring(`Example: "Test requirement" -> Technical|Operational`))
				Expect(result).To(ContainSubstring(`Example: "Another one" -> Procedural|Strategic`))
			})

			It("returns empty string for nil examples", func() {
				result := classify.ExportFormatFewShotExamples(nil)
				Expect(result).To(BeEmpty())
			})
		})

		Context("substitutePlaceholders", func() {
			It("replaces ${name} patterns", func() {
				result, err := classify.ExportSubstitutePlaceholders("Hello ${name}!", map[string]string{"name": "world"})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal("Hello world!"))
			})

			It("returns error for undefined placeholders", func() {
				_, err := classify.ExportSubstitutePlaceholders("Hello ${undefined}!", map[string]string{})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("undefined"))
			})
		})
	})

	Describe("telemetry integration", func() {
		It("creates spans on GenerateWork", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "Test requirement.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "classify.GenerateWork")
			Expect(span).NotTo(BeNil())

			tenantAttr, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(tenantAttr.AsString()).To(Equal("test-tenant"))

			controlAttr, ok := telemetrytest.SpanAttribute(span, "control.id")
			Expect(ok).To(BeTrue())
			Expect(controlAttr.AsString()).To(Equal("nist-800-53/AC-1"))
		})

		It("creates spans on Aggregate", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			_, err = a.Aggregate(ctx, []analyzer.TaskResult{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "classify.Aggregate")
			Expect(span).NotTo(BeNil())
		})

		It("records skipped attribute for section controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC",
				Statement: "ACCESS CONTROL",
				Parts:     map[string]string{"class": "compliance-section"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "classify.GenerateWork")
			Expect(span).NotTo(BeNil())

			skippedAttr, ok := telemetrytest.SpanAttribute(span, "classification.skipped")
			Expect(ok).To(BeTrue())
			Expect(skippedAttr.AsBool()).To(BeTrue())
		})
	})

	Describe("metrics integration", func() {
		It("records text length histogram for requirement controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "The organization shall define access control policy.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			histMetric := telemetrytest.FindMetric(rm, "classify.prompt.text_length")
			Expect(histMetric).NotTo(BeNil(), "expected classify.prompt.text_length metric")

			hist, ok := histMetric.Data.(metricdata.Histogram[float64])
			Expect(ok).To(BeTrue(), "expected Histogram[float64] data type")
			Expect(hist.DataPoints).NotTo(BeEmpty())
			Expect(hist.DataPoints[0].Count).To(Equal(uint64(1)))
		})

		It("records skipped counter for section controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC",
				Statement: "ACCESS CONTROL",
				Parts:     map[string]string{"class": "compliance-section"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "classify.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected classify.operations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
		})

		It("records pending counter for requirement controls", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "The organization shall define access control policy.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			_, err = a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "classify.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected classify.operations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
		})

		It("records classified and error counters in Aggregate", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(ctx) }()

			a = classify.New(mockClient, prompts, cfg, classify.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			results := []analyzer.TaskResult{
				{
					TaskID:   "task-1",
					TaskType: "classify",
					Result: &pb.AnalysisResult{
						ResultType: "classification",
						Attributes: map[string]string{
							"control_id": "AC-1",
							"type":       "Technical",
							"level":      "Operational",
						},
						Confidence: 1.0,
					},
				},
				{
					TaskID:   "task-2",
					TaskType: "classify",
					Result: &pb.AnalysisResult{
						ResultType: "classification",
						Attributes: map[string]string{
							"control_id": "AC-2",
							"type":       "Procedural",
							"level":      "Strategic",
						},
						Confidence: 1.0,
					},
				},
				{
					TaskID:   "task-3",
					TaskType: "classify",
					Error:    errors.New("LLM timeout"),
				},
			}

			_, err = a.Aggregate(ctx, results)
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "classify.operations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected classify.operations.total metric")

			// Total should be 3: 2 classified + 1 error
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))
		})

		It("does not record metrics when telemetry is not configured", func() {
			a = classify.New(mockClient, prompts, cfg) // no WithTelemetry

			control := &pb.Control{
				ControlId: "nist-800-53/AC-1",
				Statement: "Test requirement.",
				Parts:     map[string]string{"class": "compliance-requirement"},
			}
			// Should not panic with nil instruments.
			_, err := a.GenerateWork(ctx, control, analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())

			_, err = a.Aggregate(ctx, []analyzer.TaskResult{})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("registry integration", func() {
		It("registers in the analyzer registry", func() {
			r := analyzer.NewRegistry()
			err := analyzer.Register[*pb.Control](r, a)
			Expect(err).NotTo(HaveOccurred())

			got, err := r.Get("classify")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Name()).To(Equal("classify"))
			Expect(got.DependsOn()).To(BeEmpty())
		})
	})
})
