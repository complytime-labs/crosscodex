package natsbus_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

func TestNATSBusBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "NATSBus BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("NATSBus System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting NATSBus BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("NATSBus BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" — what business behaviors the NATS bus supports
	// =================================================================

	Describe("Tenant-Scoped Messaging Behaviors", func() {
		Context("when constructing tenant-scoped NATS subjects", func() {
			It("builds pipeline stage subjects for compliance workflow orchestration", func() {
				By("constructing a valid pipeline started subject")
				subject, err := natsbus.PipelineStageSubject("acme-corp", "job-001", natsbus.StageStarted)
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.pipeline.acme-corp.job-001.stage.started"))

				By("constructing a valid pipeline completed subject")
				subject, err = natsbus.PipelineStageSubject("acme-corp", "job-001", natsbus.StageCompleted)
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.pipeline.acme-corp.job-001.stage.completed"))

				By("constructing a valid pipeline failed subject")
				subject, err = natsbus.PipelineStageSubject("acme-corp", "job-001", natsbus.StageFailed)
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.pipeline.acme-corp.job-001.stage.failed"))
			})

			It("enforces tenant isolation in subject construction to prevent cross-tenant message leaks", func() {
				By("rejecting empty tenant IDs")
				_, err := natsbus.PipelineStageSubject("", "job-001", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())

				By("rejecting uppercase tenant IDs that bypass normalization")
				_, err = natsbus.PipelineStageSubject("UPPERCASE", "job-001", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())
			})

			It("prevents NATS subject injection through job ID validation", func() {
				By("rejecting empty job IDs")
				_, err := natsbus.PipelineStageSubject("acme-corp", "", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())

				By("rejecting job IDs containing NATS delimiter '.'")
				_, err = natsbus.PipelineStageSubject("acme-corp", "job.001", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())

				By("rejecting job IDs containing NATS wildcard '*'")
				_, err = natsbus.PipelineStageSubject("acme-corp", "job*001", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())

				By("rejecting job IDs containing NATS wildcard '>'")
				_, err = natsbus.PipelineStageSubject("acme-corp", "job>001", natsbus.StageStarted)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())
			})
		})

		Context("when distributing LLM work tasks across workers", func() {
			It("constructs work distribution subjects for all task types", func() {
				taskTypes := []struct {
					taskType natsbus.TaskType
					label    string
				}{
					{natsbus.TaskClassify, "classify"},
					{natsbus.TaskRelate, "relate"},
					{natsbus.TaskRequires, "requires"},
					{natsbus.TaskArtifacts, "artifacts"},
					{natsbus.TaskEmbed, "embed"},
				}

				for _, tt := range taskTypes {
					By("building work subject for task type: " + tt.label)
					subject, err := natsbus.WorkSubject("acme-corp", tt.taskType, "job-001")
					Expect(err).NotTo(HaveOccurred())
					Expect(subject).To(Equal("crosscodex.work.acme-corp." + tt.label + ".job-001"))
				}
			})

			It("rejects work subjects with invalid tenants to maintain isolation", func() {
				_, err := natsbus.WorkSubject("BAD", natsbus.TaskClassify, "job-001")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())
			})
		})

		Context("when managing compliance audit trails", func() {
			It("constructs audit subjects for each audit category", func() {
				By("building LLM audit subject")
				subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditLLM, "job-001")
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.audit.acme-corp.llm.job-001"))

				By("building decisions audit subject")
				subject, err = natsbus.AuditSubject("acme-corp", natsbus.AuditDecisions, "job-001")
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.audit.acme-corp.decisions.job-001"))

				By("building events audit subject")
				subject, err = natsbus.AuditSubject("acme-corp", natsbus.AuditEvents, "job-001")
				Expect(err).NotTo(HaveOccurred())
				Expect(subject).To(Equal("crosscodex.audit.acme-corp.events.job-001"))
			})

			It("rejects audit subjects with invalid tenants", func() {
				_, err := natsbus.AuditSubject("x", natsbus.AuditLLM, "job-001")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrInvalidSubject)).To(BeTrue())
			})
		})

		Context("when tracking message provenance for compliance attestation", func() {
			It("injects mandatory provenance headers for audit trails", func() {
				ctx := context.Background()
				data := []byte(`{"control":"AC-1","mapping":"full"}`)

				headers := natsbus.InjectProvenance(ctx, data, "test-tenant")

				By("including the tenant identity header")
				Expect(headers["X-Tenant-Id"]).To(HaveLen(1))
				Expect(headers["X-Tenant-Id"][0]).To(Equal("test-tenant"))

				By("including a SHA-256 content hash for integrity verification")
				h := sha256.Sum256(data)
				wantHash := hex.EncodeToString(h[:])
				Expect(headers["X-Content-SHA256"]).To(HaveLen(1))
				Expect(headers["X-Content-SHA256"][0]).To(Equal(wantHash))

				By("including a valid RFC3339Nano timestamp")
				Expect(headers["X-Timestamp"]).To(HaveLen(1))
				_, parseErr := time.Parse(time.RFC3339Nano, headers["X-Timestamp"][0])
				Expect(parseErr).NotTo(HaveOccurred())

				By("including OpenTelemetry trace context headers")
				Expect(headers).To(HaveKey("X-Trace-Id"))
				Expect(headers).To(HaveKey("X-Span-Id"))
			})

			It("produces deterministic content hashes for the same payload", func() {
				data := []byte("deterministic content")
				ctx := context.Background()

				h1 := natsbus.InjectProvenance(ctx, data, "tenant-aaa")
				h2 := natsbus.InjectProvenance(ctx, data, "tenant-aaa")

				Expect(h1["X-Content-SHA256"][0]).To(Equal(h2["X-Content-SHA256"][0]))
			})

			It("rejects messages with missing provenance to enforce compliance", func() {
				_, err := natsbus.ExtractProvenance(map[string][]string{})
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, natsbus.ErrMissingProvenance)).To(BeTrue())

				By("listing all missing fields in the error for actionable remediation")
				errMsg := err.Error()
				for _, field := range []string{"X-Tenant-Id", "X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"} {
					Expect(errMsg).To(ContainSubstring(field))
				}
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// These specs test the "how" — that NATS bus components follow CrossCodex contracts
	// =================================================================

	Describe("Configuration Compliance", func() {
		Context("when applying functional options", func() {
			It("uses secure defaults that follow CrossCodex conventions", func() {
				opts := natsbus.DefaultClientOptions()

				By("defaulting connect timeout to 5 seconds")
				Expect(opts.ConnectTimeout()).To(Equal(5 * time.Second))

				By("defaulting reconnect wait to 2 seconds")
				Expect(opts.ReconnectWait()).To(Equal(2 * time.Second))

				By("defaulting max reconnects to 60")
				Expect(opts.MaxReconnects()).To(Equal(60))

				By("providing a non-nil logger by default")
				Expect(opts.Logger()).NotTo(BeNil())

				By("not enabling TLS by default (explicit opt-in)")
				Expect(opts.TLSConfig()).To(BeNil())
			})

			It("allows overriding all options via functional option pattern", func() {
				logger := testspecs.GinkgoLogger()
				tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

				opts := natsbus.DefaultClientOptions()
				for _, o := range []natsbus.Option{
					natsbus.WithLogger(logger),
					natsbus.WithConnectTimeout(10 * time.Second),
					natsbus.WithReconnectWait(5 * time.Second),
					natsbus.WithMaxReconnects(100),
					natsbus.WithTLSConfig(tlsCfg),
				} {
					natsbus.ApplyOption(o, &opts)
				}

				Expect(opts.ConnectTimeout()).To(Equal(10 * time.Second))
				Expect(opts.ReconnectWait()).To(Equal(5 * time.Second))
				Expect(opts.MaxReconnects()).To(Equal(100))
				Expect(opts.Logger()).To(BeIdenticalTo(logger))
				Expect(opts.TLSConfig()).To(BeIdenticalTo(tlsCfg))
			})
		})

		Context("when configuring JetStream audit streams", func() {
			It("provisions three audit streams with correct retention policies", func() {
				streamsCfg := config.NATSStreamsConfig{
					AuditLLMRetention:    2160 * time.Hour,
					AuditEventsRetention: 720 * time.Hour,
				}

				configs := natsbus.AuditStreamConfigs(streamsCfg)
				Expect(configs).To(HaveLen(3))

				By("configuring AUDIT_LLM with 90-day retention")
				Expect(configs[0].Name).To(Equal("AUDIT_LLM"))
				Expect(configs[0].Subjects).To(ConsistOf("crosscodex.audit.*.llm.>"))
				Expect(configs[0].MaxAge).To(Equal(2160 * time.Hour))

				By("configuring AUDIT_DECISIONS with indefinite retention")
				Expect(configs[1].Name).To(Equal("AUDIT_DECISIONS"))
				Expect(configs[1].Subjects).To(ConsistOf("crosscodex.audit.*.decisions.>"))
				Expect(configs[1].MaxAge).To(Equal(time.Duration(0)))

				By("configuring AUDIT_EVENTS with 30-day retention")
				Expect(configs[2].Name).To(Equal("AUDIT_EVENTS"))
				Expect(configs[2].Subjects).To(ConsistOf("crosscodex.audit.*.events.>"))
				Expect(configs[2].MaxAge).To(Equal(720 * time.Hour))
			})
		})
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES AND INTEGRATION SCENARIOS
	// These specs test the "what" — comprehensive coverage of technical scenarios
	// =================================================================

	Describe("Subject Builder Edge Cases", func() {
		Context("when building PipelineStageSubject", func() {
			type subjectCase struct {
				tenant  string
				jobID   string
				stage   natsbus.Stage
				want    string
				wantErr error
			}

			DescribeTable("validates inputs and constructs subjects correctly",
				func(tc subjectCase) {
					got, err := natsbus.PipelineStageSubject(tc.tenant, tc.jobID, tc.stage)
					if tc.wantErr != nil {
						Expect(err).To(HaveOccurred())
						Expect(errors.Is(err, tc.wantErr)).To(BeTrue())
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(got).To(Equal(tc.want))
					}
				},
				Entry("valid started", subjectCase{
					tenant: "acme-corp", jobID: "job-001", stage: natsbus.StageStarted,
					want: "crosscodex.pipeline.acme-corp.job-001.stage.started",
				}),
				Entry("valid completed", subjectCase{
					tenant: "acme-corp", jobID: "job-001", stage: natsbus.StageCompleted,
					want: "crosscodex.pipeline.acme-corp.job-001.stage.completed",
				}),
				Entry("valid failed", subjectCase{
					tenant: "acme-corp", jobID: "job-001", stage: natsbus.StageFailed,
					want: "crosscodex.pipeline.acme-corp.job-001.stage.failed",
				}),
				Entry("empty tenant", subjectCase{
					tenant: "", jobID: "job-001", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("invalid tenant", subjectCase{
					tenant: "UPPERCASE", jobID: "job-001", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("empty job ID", subjectCase{
					tenant: "acme-corp", jobID: "", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("job ID with dot", subjectCase{
					tenant: "acme-corp", jobID: "job.001", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("job ID with wildcard star", subjectCase{
					tenant: "acme-corp", jobID: "job*001", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("job ID with wildcard gt", subjectCase{
					tenant: "acme-corp", jobID: "job>001", stage: natsbus.StageStarted,
					wantErr: natsbus.ErrInvalidSubject,
				}),
			)
		})

		Context("when building PipelineStateSubject", func() {
			It("constructs a valid pipeline state subject", func() {
				got, err := natsbus.PipelineStateSubject("acme-corp", "job-001")
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal("crosscodex.pipeline.acme-corp.job-001.state"))
			})
		})

		Context("when building WorkSubject", func() {
			type workCase struct {
				tenant   string
				taskType natsbus.TaskType
				jobID    string
				want     string
				wantErr  error
			}

			DescribeTable("validates inputs and constructs work subjects correctly",
				func(tc workCase) {
					got, err := natsbus.WorkSubject(tc.tenant, tc.taskType, tc.jobID)
					if tc.wantErr != nil {
						Expect(err).To(HaveOccurred())
						Expect(errors.Is(err, tc.wantErr)).To(BeTrue())
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(got).To(Equal(tc.want))
					}
				},
				Entry("classify", workCase{
					tenant: "acme-corp", taskType: natsbus.TaskClassify, jobID: "job-001",
					want: "crosscodex.work.acme-corp.classify.job-001",
				}),
				Entry("relate", workCase{
					tenant: "acme-corp", taskType: natsbus.TaskRelate, jobID: "job-001",
					want: "crosscodex.work.acme-corp.relate.job-001",
				}),
				Entry("requires", workCase{
					tenant: "acme-corp", taskType: natsbus.TaskRequires, jobID: "job-001",
					want: "crosscodex.work.acme-corp.requires.job-001",
				}),
				Entry("artifacts", workCase{
					tenant: "acme-corp", taskType: natsbus.TaskArtifacts, jobID: "job-001",
					want: "crosscodex.work.acme-corp.artifacts.job-001",
				}),
				Entry("embed", workCase{
					tenant: "acme-corp", taskType: natsbus.TaskEmbed, jobID: "job-001",
					want: "crosscodex.work.acme-corp.embed.job-001",
				}),
				Entry("invalid tenant", workCase{
					tenant: "BAD", taskType: natsbus.TaskClassify, jobID: "job-001",
					wantErr: natsbus.ErrInvalidSubject,
				}),
			)
		})

		Context("when building ResultSubject", func() {
			It("constructs a valid result subject", func() {
				got, err := natsbus.ResultSubject("acme-corp", natsbus.TaskClassify, "job-001")
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal("crosscodex.results.acme-corp.classify.job-001"))
			})
		})

		Context("when building AuditSubject", func() {
			type auditCase struct {
				tenant    string
				auditType natsbus.AuditType
				jobID     string
				want      string
				wantErr   error
			}

			DescribeTable("validates inputs and constructs audit subjects correctly",
				func(tc auditCase) {
					got, err := natsbus.AuditSubject(tc.tenant, tc.auditType, tc.jobID)
					if tc.wantErr != nil {
						Expect(err).To(HaveOccurred())
						Expect(errors.Is(err, tc.wantErr)).To(BeTrue())
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(got).To(Equal(tc.want))
					}
				},
				Entry("llm audit", auditCase{
					tenant: "acme-corp", auditType: natsbus.AuditLLM, jobID: "job-001",
					want: "crosscodex.audit.acme-corp.llm.job-001",
				}),
				Entry("decisions audit", auditCase{
					tenant: "acme-corp", auditType: natsbus.AuditDecisions, jobID: "job-001",
					want: "crosscodex.audit.acme-corp.decisions.job-001",
				}),
				Entry("events audit", auditCase{
					tenant: "acme-corp", auditType: natsbus.AuditEvents, jobID: "job-001",
					want: "crosscodex.audit.acme-corp.events.job-001",
				}),
				Entry("invalid tenant", auditCase{
					tenant: "x", auditType: natsbus.AuditLLM, jobID: "job-001",
					wantErr: natsbus.ErrInvalidSubject,
				}),
			)
		})

		Context("when building FeedbackSubject", func() {
			type feedbackCase struct {
				tenant  string
				edgeID  string
				want    string
				wantErr error
			}

			DescribeTable("validates inputs and constructs feedback subjects correctly",
				func(tc feedbackCase) {
					got, err := natsbus.FeedbackSubject(tc.tenant, tc.edgeID)
					if tc.wantErr != nil {
						Expect(err).To(HaveOccurred())
						Expect(errors.Is(err, tc.wantErr)).To(BeTrue())
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(got).To(Equal(tc.want))
					}
				},
				Entry("valid feedback", feedbackCase{
					tenant: "acme-corp", edgeID: "edge-abc-123",
					want: "crosscodex.feedback.acme-corp.edge-abc-123",
				}),
				Entry("empty edge ID", feedbackCase{
					tenant: "acme-corp", edgeID: "",
					wantErr: natsbus.ErrInvalidSubject,
				}),
				Entry("edge ID with dot", feedbackCase{
					tenant: "acme-corp", edgeID: "edge.bad",
					wantErr: natsbus.ErrInvalidSubject,
				}),
			)
		})
	})

	Describe("Provenance Edge Cases", func() {
		Context("when extracting provenance from complete headers", func() {
			It("extracts all provenance fields from valid headers", func() {
				data := []byte("test payload")
				h := sha256.Sum256(data)
				wantHash := hex.EncodeToString(h[:])
				ts := time.Now().UTC()

				headers := map[string][]string{
					"X-Trace-Id":       {"abc123"},
					"X-Span-Id":        {"span456"},
					"X-Tenant-Id":      {"acme-corp"},
					"X-Timestamp":      {ts.Format(time.RFC3339Nano)},
					"X-Content-SHA256": {wantHash},
				}

				meta, err := natsbus.ExtractProvenance(headers)
				Expect(err).NotTo(HaveOccurred())
				Expect(meta.TraceID).To(Equal("abc123"))
				Expect(meta.SpanID).To(Equal("span456"))
				Expect(meta.TenantID).To(Equal("acme-corp"))
				Expect(meta.ContentHash).To(Equal(wantHash))
			})
		})

		Context("when extracting provenance from partial headers", func() {
			type partialCase struct {
				headers        map[string][]string
				wantMissing    []string
				wantNotMissing []string
			}

			DescribeTable("reports exactly which fields are missing",
				func(tc partialCase) {
					_, err := natsbus.ExtractProvenance(tc.headers)
					Expect(err).To(HaveOccurred())
					Expect(errors.Is(err, natsbus.ErrMissingProvenance)).To(BeTrue())

					errMsg := err.Error()
					for _, field := range tc.wantMissing {
						Expect(errMsg).To(ContainSubstring(field))
					}
					for _, field := range tc.wantNotMissing {
						Expect(errMsg).NotTo(ContainSubstring(field))
					}
				},
				Entry("only tenant present", partialCase{
					headers: map[string][]string{
						"X-Tenant-Id": {"acme-corp"},
					},
					wantMissing:    []string{"X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
					wantNotMissing: []string{"X-Tenant-Id"},
				}),
				Entry("everything except tenant", partialCase{
					headers: map[string][]string{
						"X-Trace-Id":       {"trace-1"},
						"X-Span-Id":        {"span-1"},
						"X-Timestamp":      {"2026-01-01T00:00:00Z"},
						"X-Content-SHA256": {"abc123"},
					},
					wantMissing:    []string{"X-Tenant-Id"},
					wantNotMissing: []string{"X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
				}),
				Entry("nil headers map", partialCase{
					headers:        nil,
					wantMissing:    []string{"X-Tenant-Id", "X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
					wantNotMissing: nil,
				}),
				Entry("empty string values treated as missing", partialCase{
					headers: map[string][]string{
						"X-Tenant-Id":      {""},
						"X-Trace-Id":       {""},
						"X-Span-Id":        {"valid-span"},
						"X-Timestamp":      {"2026-01-01T00:00:00Z"},
						"X-Content-SHA256": {"abc123"},
					},
					wantMissing:    []string{"X-Tenant-Id", "X-Trace-Id"},
					wantNotMissing: []string{"X-Span-Id", "X-Timestamp", "X-Content-SHA256"},
				}),
				Entry("empty slice values treated as missing", partialCase{
					headers: map[string][]string{
						"X-Tenant-Id":      {},
						"X-Trace-Id":       {"valid-trace"},
						"X-Span-Id":        {"valid-span"},
						"X-Timestamp":      {"2026-01-01T00:00:00Z"},
						"X-Content-SHA256": {"abc123"},
					},
					wantMissing:    []string{"X-Tenant-Id"},
					wantNotMissing: []string{"X-Trace-Id", "X-Span-Id", "X-Timestamp", "X-Content-SHA256"},
				}),
			)
		})

		Context("when extracting provenance from multi-valued headers", func() {
			It("uses only the first value for each header", func() {
				headers := map[string][]string{
					"X-Trace-Id":       {"first-trace", "second-trace"},
					"X-Span-Id":        {"first-span", "second-span"},
					"X-Tenant-Id":      {"first-tenant", "second-tenant"},
					"X-Timestamp":      {"2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"},
					"X-Content-SHA256": {"hash1", "hash2"},
				}

				meta, err := natsbus.ExtractProvenance(headers)
				Expect(err).NotTo(HaveOccurred())
				Expect(meta.TraceID).To(Equal("first-trace"))
				Expect(meta.TenantID).To(Equal("first-tenant"))
			})
		})
	})

	Describe("Header Merge Edge Cases", func() {
		Context("when merging user headers with provenance headers", func() {
			It("preserves user headers and lets provenance take precedence on conflict", func() {
				base := map[string][]string{
					"X-Custom":    {"value1"},
					"X-Tenant-Id": {"should-be-overwritten"},
				}

				provenance := map[string][]string{
					"X-Tenant-Id": {"correct-tenant"},
					"X-Trace-Id":  {"trace-001"},
				}

				merged := natsbus.MergeHeaders(base, provenance)

				By("preserving non-conflicting user headers")
				Expect(merged["X-Custom"]).To(HaveLen(1))
				Expect(merged["X-Custom"][0]).To(Equal("value1"))

				By("overwriting conflicting keys with provenance values")
				Expect(merged["X-Tenant-Id"]).To(HaveLen(1))
				Expect(merged["X-Tenant-Id"][0]).To(Equal("correct-tenant"))

				By("including new provenance-only headers")
				Expect(merged["X-Trace-Id"]).To(HaveLen(1))
				Expect(merged["X-Trace-Id"][0]).To(Equal("trace-001"))
			})
		})
	})

	Describe("XDG State Directory Edge Cases", func() {
		Context("when resolving the NATS state directory", func() {
			It("falls back to $HOME/.local/state when XDG_STATE_HOME is unset", func() {
				GinkgoT().Setenv("XDG_STATE_HOME", "")

				got := natsbus.XDGNATSStateDir()
				wantEnd := filepath.Join(".local", "state", "crosscodex", "nats")
				Expect(got).To(HaveSuffix(wantEnd))
			})

			It("uses XDG_STATE_HOME when set", func() {
				GinkgoT().Setenv("XDG_STATE_HOME", "/custom/state")

				got := natsbus.XDGNATSStateDir()
				wantEnd := filepath.Join("crosscodex", "nats")
				Expect(got).To(HaveSuffix(wantEnd))
				Expect(got).To(HavePrefix("/custom/state"))
			})
		})
	})

	Describe("Store Directory Resolution Edge Cases", func() {
		Context("when resolving the JetStream store directory", func() {
			It("uses the configured directory when provided", func() {
				cfg := config.NATSEmbeddedConfig{
					StoreDir: "/tmp/nats-test-store",
				}
				got := natsbus.ResolveStoreDir(cfg)
				Expect(got).To(Equal("/tmp/nats-test-store"))
			})

			It("falls back to XDG state directory when unconfigured", func() {
				cfg := config.NATSEmbeddedConfig{
					StoreDir: "",
				}
				got := natsbus.ResolveStoreDir(cfg)
				Expect(got).NotTo(BeEmpty())
			})
		})
	})

	Describe("Embedded Mode Detection Edge Cases", func() {
		Context("when determining connection mode", func() {
			It("detects embedded mode from empty URL", func() {
				Expect(natsbus.IsEmbeddedMode("")).To(BeTrue())
			})

			It("detects external mode from non-empty URL", func() {
				Expect(natsbus.IsEmbeddedMode("nats://localhost:4222")).To(BeFalse())
			})
		})
	})
})
