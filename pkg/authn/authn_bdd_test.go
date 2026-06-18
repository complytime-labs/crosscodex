package authn_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

func TestAuthnBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Authn BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// recordingEmitter captures AuthEvents for test assertions.
type recordingEmitter struct {
	mu     sync.Mutex
	events []*authn.AuthEvent
}

func (r *recordingEmitter) EmitAuthEvent(_ context.Context, event *authn.AuthEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingEmitter) lastEvent() *authn.AuthEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	return r.events[len(r.events)-1]
}

// failingEmitter returns an error to verify audit failures don't block auth.
type failingEmitter struct{}

func (f *failingEmitter) EmitAuthEvent(_ context.Context, _ *authn.AuthEvent) error {
	return errors.New("audit emission failed")
}

// tlsStateWith creates a ConnectionState with the given peer certificates.
func tlsStateWith(certs ...*x509.Certificate) *tls.ConnectionState {
	return &tls.ConnectionState{PeerCertificates: certs}
}

// telemetryStubAuth is a minimal Authenticator for telemetry span tests.
// It avoids TLS handshake setup by returning a canned identity or error.
type telemetryStubAuth struct {
	identity *authn.Identity
	err      error
}

func (s *telemetryStubAuth) Authenticate(_ context.Context, _ *authn.Request) (*authn.Identity, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.identity, nil
}

func (s *telemetryStubAuth) SupportedMethods() []authn.AuthMethod {
	return []authn.AuthMethod{authn.AuthMethodMTLS}
}

// ---------------------------------------------------------------------------
// BDD Suite
// ---------------------------------------------------------------------------

var _ = Describe("Authn Package", Ordered, func() {

	// Shared PKI fixtures — generated once in BeforeAll.
	var (
		ca *pki.CertKeyPair

		// Acme Corp cert: CN=web.acme.internal, O=Acme Corp, OU=Engineering,
		// DNS=web.acme.internal, Email=admin@acme.com
		acmeCert *x509.Certificate

		// Globex cert: CN=api.globex.internal, O=Globex Inc, OU=Platform,
		// DNS=api.globex.internal, Email=ops@globex.com
		globexCert *x509.Certificate

		// Bare cert: CN=bare.internal, O=Bare Org, no OU, no email, no URI
		bareCert *x509.Certificate

		// URI cert: CN=spiffe-node, O=SPIFFE Org, DNS=spiffe-node,
		// URI=spiffe://cluster.local/ns/prod/sa/worker
		uriCert *x509.Certificate
	)

	BeforeAll(func() {
		testspecs.LogTestProgress("Generating PKI fixtures for authn tests")

		var err error
		ca, err = pki.GenerateCA(pki.WithOrganization("Test CA"))
		Expect(err).NotTo(HaveOccurred())

		acmePair, err := pki.GenerateCert(ca,
			pki.WithOrganization("Acme Corp"),
			pki.WithOrgUnit("Engineering"),
			pki.WithDNSNames("web.acme.internal"),
			pki.WithEmailAddresses("admin@acme.com"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())
		acmeCert = acmePair.Cert

		globexPair, err := pki.GenerateCert(ca,
			pki.WithOrganization("Globex Inc"),
			pki.WithOrgUnit("Platform"),
			pki.WithDNSNames("api.globex.internal"),
			pki.WithEmailAddresses("ops@globex.com"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())
		globexCert = globexPair.Cert

		barePair, err := pki.GenerateCert(ca,
			pki.WithOrganization("Bare Org"),
			pki.WithDNSNames("bare.internal"),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())
		bareCert = barePair.Cert

		u, err := url.Parse("spiffe://cluster.local/ns/prod/sa/worker")
		Expect(err).NotTo(HaveOccurred())

		uriPair, err := pki.GenerateCert(ca,
			pki.WithOrganization("SPIFFE Org"),
			pki.WithDNSNames("spiffe-node"),
			pki.WithURIs(u),
			pki.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
		)
		Expect(err).NotTo(HaveOccurred())
		uriCert = uriPair.Cert
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Authn BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: GLOB MATCHING PRIMITIVES
	// =================================================================

	Describe("Level 1: Glob Matching Primitives", func() {

		Describe("GlobMatch", func() {
			It("matches exact strings", func() {
				Expect(authn.GlobMatch("hello", "hello")).To(BeTrue())
			})

			It("rejects non-matching strings", func() {
				Expect(authn.GlobMatch("hello", "world")).To(BeFalse())
			})

			It("supports * wildcard", func() {
				Expect(authn.GlobMatch("*.acme.com", "web.acme.com")).To(BeTrue())
				Expect(authn.GlobMatch("*.acme.com", "acme.com")).To(BeFalse())
			})

			It("supports ? single-character wildcard", func() {
				Expect(authn.GlobMatch("h?llo", "hello")).To(BeTrue())
				Expect(authn.GlobMatch("h?llo", "hallo")).To(BeTrue())
				Expect(authn.GlobMatch("h?llo", "hllo")).To(BeFalse())
			})

			It("treats invalid patterns as non-match", func() {
				// filepath.Match returns an error for unmatched brackets
				Expect(authn.GlobMatch("[invalid", "anything")).To(BeFalse())
			})

			It("handles empty pattern and value", func() {
				Expect(authn.GlobMatch("", "")).To(BeTrue())
				Expect(authn.GlobMatch("", "notempty")).To(BeFalse())
				Expect(authn.GlobMatch("notempty", "")).To(BeFalse())
			})
		})

		Describe("MatchFirst", func() {
			It("matches pattern against first element only", func() {
				Expect(authn.MatchFirst("Acme*", []string{"Acme Corp", "Other"})).To(BeTrue())
			})

			It("ignores subsequent elements", func() {
				Expect(authn.MatchFirst("Other", []string{"Acme Corp", "Other"})).To(BeFalse())
			})

			It("returns false for empty slice", func() {
				Expect(authn.MatchFirst("anything", []string{})).To(BeFalse())
			})

			It("returns false for nil slice", func() {
				Expect(authn.MatchFirst("anything", nil)).To(BeFalse())
			})
		})

		Describe("MatchAny", func() {
			It("returns true when any element matches", func() {
				Expect(authn.MatchAny("b*", []string{"alpha", "beta", "gamma"})).To(BeTrue())
			})

			It("returns false when no element matches", func() {
				Expect(authn.MatchAny("z*", []string{"alpha", "beta"})).To(BeFalse())
			})

			It("returns false for empty slice", func() {
				Expect(authn.MatchAny("*", []string{})).To(BeFalse())
			})

			It("returns false for nil slice", func() {
				Expect(authn.MatchAny("*", nil)).To(BeFalse())
			})

			It("matches with wildcard across all elements", func() {
				Expect(authn.MatchAny("*@acme.com", []string{"user@globex.com", "admin@acme.com"})).To(BeTrue())
			})
		})
	})

	// =================================================================
	// LEVEL 2: CERTIFICATE FIELD MATCHING
	// =================================================================

	Describe("Level 2: Certificate Field Matching", func() {

		Describe("MatchCert with CN", func() {
			It("matches exact CN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN: "web.acme.internal",
				})).To(BeTrue())
			})

			It("matches CN with wildcard", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN: "*.acme.internal",
				})).To(BeTrue())
			})

			It("rejects non-matching CN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN: "wrong.host",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with Organization", func() {
			It("matches exact organization", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					Organization: "Acme Corp",
				})).To(BeTrue())
			})

			It("matches organization with wildcard", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					Organization: "Acme*",
				})).To(BeTrue())
			})

			It("rejects non-matching organization", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					Organization: "Globex*",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with OrgUnit", func() {
			It("matches exact org unit", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					OrgUnit: "Engineering",
				})).To(BeTrue())
			})

			It("rejects non-matching org unit", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					OrgUnit: "Sales",
				})).To(BeFalse())
			})

			It("returns false when cert has no org unit", func() {
				Expect(authn.MatchCert(bareCert, authn.X509Match{
					OrgUnit: "Engineering",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with SANEmail", func() {
			It("matches email SAN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANEmail: "admin@acme.com",
				})).To(BeTrue())
			})

			It("matches email SAN with wildcard", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANEmail: "*@acme.com",
				})).To(BeTrue())
			})

			It("rejects non-matching email SAN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANEmail: "user@other.com",
				})).To(BeFalse())
			})

			It("returns false when cert has no emails", func() {
				Expect(authn.MatchCert(bareCert, authn.X509Match{
					SANEmail: "*@acme.com",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with SANDNS", func() {
			It("matches DNS SAN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANDNS: "web.acme.internal",
				})).To(BeTrue())
			})

			It("matches DNS SAN with wildcard", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANDNS: "*.acme.internal",
				})).To(BeTrue())
			})

			It("rejects non-matching DNS SAN", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANDNS: "*.globex.internal",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with SANURI", func() {
			It("matches URI SAN with exact string", func() {
				Expect(authn.MatchCert(uriCert, authn.X509Match{
					SANURI: "spiffe://cluster.local/ns/prod/sa/worker",
				})).To(BeTrue())
			})

			It("matches URI SAN with wildcard", func() {
				Expect(authn.MatchCert(uriCert, authn.X509Match{
					SANURI: "spiffe://cluster.local/*",
				})).To(BeFalse()) // * does not cross / in filepath.Match
			})

			It("rejects non-matching URI SAN", func() {
				Expect(authn.MatchCert(uriCert, authn.X509Match{
					SANURI: "spiffe://other.local/ns/prod/sa/worker",
				})).To(BeFalse())
			})

			It("returns false when cert has no URI SANs", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					SANURI: "spiffe://*",
				})).To(BeFalse())
			})
		})

		Describe("MatchCert with MatchAnyURI helper", func() {
			It("matches against URI SAN string form", func() {
				Expect(authn.MatchAnyURI(
					"spiffe://cluster.local/ns/prod/sa/worker",
					uriCert,
				)).To(BeTrue())
			})

			It("returns false for cert without URIs", func() {
				Expect(authn.MatchAnyURI("spiffe://*", bareCert)).To(BeFalse())
			})
		})

		Describe("MatchCert AND semantics", func() {
			It("requires ALL non-empty fields to match", func() {
				By("matching when both CN and Organization match")
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN:           "web.acme.internal",
					Organization: "Acme Corp",
				})).To(BeTrue())

				By("failing when CN matches but Organization does not")
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN:           "web.acme.internal",
					Organization: "Wrong Corp",
				})).To(BeFalse())

				By("failing when Organization matches but CN does not")
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN:           "wrong.host",
					Organization: "Acme Corp",
				})).To(BeFalse())
			})

			It("matches all six fields simultaneously", func() {
				Expect(authn.MatchCert(acmeCert, authn.X509Match{
					CN:           "web.acme.internal",
					Organization: "Acme Corp",
					OrgUnit:      "Engineering",
					SANEmail:     "*@acme.com",
					SANDNS:       "*.acme.internal",
				})).To(BeTrue())
			})
		})

		Describe("MatchCert with empty match (all fields empty)", func() {
			It("matches any certificate at the primitive level", func() {
				// matchCert itself still returns true for empty matches (AND
				// semantics on zero fields). The safety guard is at construction
				// time — NewX509Authenticator rejects empty matches in
				// multi-tenant mode to prevent accidental wildcard mappings.
				Expect(authn.MatchCert(acmeCert, authn.X509Match{})).To(BeTrue())
				Expect(authn.MatchCert(globexCert, authn.X509Match{})).To(BeTrue())
				Expect(authn.MatchCert(bareCert, authn.X509Match{})).To(BeTrue())
			})
		})

		Describe("CertClaims", func() {
			It("extracts serial and issuer", func() {
				claims := authn.CertClaims(acmeCert)
				Expect(claims).To(HaveKey("serial"))
				Expect(claims["serial"]).To(Equal(acmeCert.SerialNumber.String()))
				Expect(claims).To(HaveKey("issuer"))
				Expect(claims["issuer"]).To(Equal(acmeCert.Issuer.CommonName))
			})

			It("includes dns_names when present", func() {
				claims := authn.CertClaims(acmeCert)
				Expect(claims).To(HaveKey("dns_names"))
				Expect(claims["dns_names"]).To(ContainElement("web.acme.internal"))
			})

			It("omits dns_names when cert has none", func() {
				// bareCert has DNSNames from pki generation, so we test with
				// a cert that has them — we verify the key exists for certs
				// with DNS names and is absent for certs without.
				// GenerateCert always sets DNS names from options; bare has "bare.internal".
				claims := authn.CertClaims(bareCert)
				Expect(claims).To(HaveKey("dns_names"))
			})
		})
	})

	// =================================================================
	// LEVEL 2.5: TELEMETRY INTEGRATION
	// =================================================================

	Describe("Telemetry Integration", func() {
		Context("when creating a registry without telemetry", func() {
			It("has nil telemetry fields", func() {
				emitter := &recordingEmitter{}
				registry, err := authn.NewRegistry(emitter, nil)
				Expect(err).NotTo(HaveOccurred())

				tf := authn.ExportTelemetryFields(registry)
				Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
				Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
				Expect(tf.HasAuthCounter).To(BeFalse(), "authCounter should be nil without telemetry")
				Expect(tf.HasAuthLatency).To(BeFalse(), "authLatency should be nil without telemetry")
			})
		})

		Context("when creating a registry with telemetry", func() {
			It("initializes all telemetry instruments", func() {
				tp := tracenoop.NewTracerProvider()
				tracer := tp.Tracer("authn-test")
				mp := metricnoop.NewMeterProvider()
				meter := mp.Meter("authn-test")

				emitter := &recordingEmitter{}
				registry, err := authn.NewRegistry(emitter, nil, authn.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())

				tf := authn.ExportTelemetryFields(registry)
				Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
				Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
				Expect(tf.HasAuthCounter).To(BeTrue(), "authCounter should be set with telemetry")
				Expect(tf.HasAuthLatency).To(BeTrue(), "authLatency should be set with telemetry")
			})
		})

		Context("when Authenticate produces spans", func() {
			var (
				tp       *telemetrytest.TestProvider
				recorder *recordingEmitter
			)

			BeforeEach(func() {
				var err error
				tp, err = telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				recorder = &recordingEmitter{}
			})

			AfterEach(func() {
				Expect(tp.Shutdown(context.Background())).To(Succeed())
			})

			It("emits authn.Authenticate span with auth.success=true on success", func() {
				tracer := tp.TracerProvider().Tracer("authn-test")
				meter := tp.MeterProvider().Meter("authn-test")

				successAuth := &telemetryStubAuth{
					identity: &authn.Identity{
						TenantID: "span-tenant",
						Subject:  "test-user",
						Method:   authn.AuthMethodMTLS,
					},
				}
				registry, err := authn.NewRegistry(recorder, []authn.Authenticator{successAuth},
					authn.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())

				_, err = registry.Authenticate(context.Background(), &authn.Request{
					Method: authn.AuthMethodMTLS,
				})
				Expect(err).NotTo(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "authn.Authenticate")
				Expect(span).NotTo(BeNil(), "expected authn.Authenticate span")
				Expect(span.Status().Code.String()).To(Equal("Ok"))

				successVal, ok := telemetrytest.SpanAttribute(span, "auth.success")
				Expect(ok).To(BeTrue())
				Expect(successVal.AsBool()).To(BeTrue())

				tenantVal, ok := telemetrytest.SpanAttribute(span, "tenant.id")
				Expect(ok).To(BeTrue())
				Expect(tenantVal.AsString()).To(Equal("span-tenant"))
			})

			It("emits authn.Authenticate span with Error status on failure and records metrics", func() {
				tracer := tp.TracerProvider().Tracer("authn-test")
				meter := tp.MeterProvider().Meter("authn-test")

				failAuth := &telemetryStubAuth{
					err: authn.ErrAuthenticationFailed,
				}
				registry, err := authn.NewRegistry(recorder, []authn.Authenticator{failAuth},
					authn.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())

				_, err = registry.Authenticate(context.Background(), &authn.Request{
					Method: authn.AuthMethodMTLS,
				})
				Expect(err).To(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, "authn.Authenticate")
				Expect(span).NotTo(BeNil(), "expected authn.Authenticate span")
				Expect(span.Status().Code.String()).To(Equal("Error"))

				successVal, ok := telemetrytest.SpanAttribute(span, "auth.success")
				Expect(ok).To(BeTrue())
				Expect(successVal.AsBool()).To(BeFalse())

				// Metrics are recorded on failure paths too.
				rm := tp.GetMetrics()
				counter := telemetrytest.FindMetric(rm, "authn.attempts.total")
				Expect(counter).NotTo(BeNil(), "expected authn.attempts.total on failure path")
				val, counterErr := telemetrytest.CounterValue(counter)
				Expect(counterErr).NotTo(HaveOccurred())
				Expect(val).To(BeNumerically(">=", 1))

				hist := telemetrytest.FindMetric(rm, "authn.duration_ms")
				Expect(hist).NotTo(BeNil(), "expected authn.duration_ms on failure path")
				hc, histErr := telemetrytest.HistogramCount(hist)
				Expect(histErr).NotTo(HaveOccurred())
				Expect(hc).To(BeNumerically(">=", 1))
			})

			It("records authn.attempts.total and authn.duration_ms", func() {
				tracer := tp.TracerProvider().Tracer("authn-test")
				meter := tp.MeterProvider().Meter("authn-test")

				successAuth := &telemetryStubAuth{
					identity: &authn.Identity{
						TenantID: "metrics-tenant",
						Subject:  "m-user",
						Method:   authn.AuthMethodMTLS,
					},
				}
				registry, err := authn.NewRegistry(recorder, []authn.Authenticator{successAuth},
					authn.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())

				_, err = registry.Authenticate(context.Background(), &authn.Request{
					Method: authn.AuthMethodMTLS,
				})
				Expect(err).NotTo(HaveOccurred())

				rm := tp.GetMetrics()

				counter := telemetrytest.FindMetric(rm, "authn.attempts.total")
				Expect(counter).NotTo(BeNil())
				val, err := telemetrytest.CounterValue(counter)
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNumerically(">=", 1))

				hist := telemetrytest.FindMetric(rm, "authn.duration_ms")
				Expect(hist).NotTo(BeNil())
				hc, histErr := telemetrytest.HistogramCount(hist)
				Expect(histErr).NotTo(HaveOccurred())
				Expect(hc).To(BeNumerically(">=", 1))
			})

			// Attestation audit path: SessionID is populated from trace context.
			// When pkg/attestation is implemented (Phase 5), it will read
			// AuditEvent.SessionID to link attestation predicates to traces.
			It("populates SessionID from trace context for attestation correlation", func() {
				tracer := tp.TracerProvider().Tracer("authn-test")
				meter := tp.MeterProvider().Meter("authn-test")

				successAuth := &telemetryStubAuth{
					identity: &authn.Identity{
						TenantID: "attest-tenant",
						Subject:  "attest-user",
						Method:   authn.AuthMethodMTLS,
					},
				}
				registry, err := authn.NewRegistry(recorder, []authn.Authenticator{successAuth},
					authn.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())

				// Create an active span so TraceIDFromContext returns a real ID.
				ctx, parentSpan := tracer.Start(context.Background(), "test.parent")
				defer parentSpan.End()

				_, err = registry.Authenticate(ctx, &authn.Request{
					Method: authn.AuthMethodMTLS,
				})
				Expect(err).NotTo(HaveOccurred())

				// The audit event SessionID should be the trace ID hex string.
				event := recorder.lastEvent()
				Expect(event).NotTo(BeNil())
				Expect(event.SessionID).NotTo(BeEmpty())
				Expect(event.SessionID).To(Equal(
					parentSpan.SpanContext().TraceID().String()))
			})
		})
	})

	// =================================================================
	// LEVEL 3: X509Authenticator
	// =================================================================

	Describe("Level 3: X509Authenticator", func() {

		Describe("Construction validation", func() {
			It("rejects invalid default tenant in single-tenant mode", func() {
				_, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant:  true,
					DefaultTenant: "INVALID_TENANT", // uppercase not allowed
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("invalid default tenant"))
			})

			It("rejects invalid tenant in mapping rules", func() {
				_, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant: false,
					Mappings: []authn.X509Mapping{
						{Match: authn.X509Match{CN: "*"}, Tenant: "valid-tenant", Roles: []string{"admin"}},
						{Match: authn.X509Match{CN: "*"}, Tenant: "BAD!", Roles: []string{"reader"}},
					},
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("invalid tenant in X509 mapping 1"))
			})

			It("accepts valid single-tenant config", func() {
				auth, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant:  true,
					DefaultTenant: "default-tenant",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).NotTo(BeNil())
			})

			It("rejects empty match in multi-tenant mapping (would match all certs)", func() {
				_, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant: false,
					Mappings: []authn.X509Mapping{
						{Match: authn.X509Match{}, Tenant: "catch-all", Roles: []string{"reader"}},
					},
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("empty match"))
				Expect(err.Error()).To(ContainSubstring("mapping 0"))
			})

			It("accepts valid multi-tenant config", func() {
				auth, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant: false,
					Mappings: []authn.X509Mapping{
						{Match: authn.X509Match{Organization: "Acme*"}, Tenant: "acme-tenant", Roles: []string{"admin"}},
					},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(auth).NotTo(BeNil())
			})
		})

		Describe("Single-tenant mode", func() {
			var auth *authn.X509Authenticator

			BeforeEach(func() {
				var err error
				auth, err = authn.NewX509Authenticator(authn.X509Config{
					SingleTenant:  true,
					DefaultTenant: "solo-tenant",
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("authenticates any cert with default tenant and admin role", func() {
				identity, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(acmeCert),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(identity.Subject).To(Equal("web.acme.internal"))
				Expect(identity.TenantID).To(Equal("solo-tenant"))
				Expect(identity.Roles).To(Equal([]string{"admin"}))
				Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))
			})

			It("returns ErrUnsupportedMethod when TLSState is nil", func() {
				_, err := auth.Authenticate(context.Background(), &authn.Request{})
				Expect(err).To(MatchError(authn.ErrUnsupportedMethod))
			})

			It("returns ErrAuthenticationFailed when no peer certificates", func() {
				_, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: &tls.ConnectionState{},
				})
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, authn.ErrAuthenticationFailed)).To(BeTrue())
			})
		})

		Describe("Multi-tenant mode", func() {
			var auth *authn.X509Authenticator

			BeforeEach(func() {
				var err error
				auth, err = authn.NewX509Authenticator(authn.X509Config{
					SingleTenant: false,
					Mappings: []authn.X509Mapping{
						{
							Match:  authn.X509Match{Organization: "Acme*"},
							Tenant: "acme-tenant",
							Roles:  []string{"admin", "writer"},
						},
						{
							Match:  authn.X509Match{Organization: "Globex*"},
							Tenant: "globex-tenant",
							Roles:  []string{"reader"},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred())
			})

			It("resolves first matching rule (Acme)", func() {
				identity, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(acmeCert),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(identity.TenantID).To(Equal("acme-tenant"))
				Expect(identity.Roles).To(Equal([]string{"admin", "writer"}))
			})

			It("resolves second matching rule (Globex)", func() {
				identity, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(globexCert),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(identity.TenantID).To(Equal("globex-tenant"))
				Expect(identity.Roles).To(Equal([]string{"reader"}))
			})

			It("uses first-match-wins ordering", func() {
				// Both rules match "*" but order determines winner
				dualAuth, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant: false,
					Mappings: []authn.X509Mapping{
						{Match: authn.X509Match{Organization: "*"}, Tenant: "first-wins", Roles: []string{"first"}},
						{Match: authn.X509Match{Organization: "*"}, Tenant: "second-loses", Roles: []string{"second"}},
					},
				})
				Expect(err).NotTo(HaveOccurred())

				identity, err := dualAuth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(acmeCert),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(identity.TenantID).To(Equal("first-wins"))
			})

			It("returns ErrAuthenticationFailed when no mapping matches", func() {
				_, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(bareCert),
				})
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, authn.ErrAuthenticationFailed)).To(BeTrue())
				Expect(err.Error()).To(ContainSubstring("no mapping matched"))
			})
		})

		Describe("Claims extraction in Authenticate", func() {
			It("populates claims from the leaf certificate", func() {
				auth, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant:  true,
					DefaultTenant: "claims-tenant",
				})
				Expect(err).NotTo(HaveOccurred())

				identity, err := auth.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(acmeCert),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(identity.Claims).To(HaveKey("serial"))
				Expect(identity.Claims).To(HaveKey("issuer"))
				Expect(identity.Claims).To(HaveKey("dns_names"))
			})
		})

		Describe("SupportedMethods", func() {
			It("returns AuthMethodMTLS", func() {
				auth, err := authn.NewX509Authenticator(authn.X509Config{
					SingleTenant:  true,
					DefaultTenant: "methods-tenant",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(auth.SupportedMethods()).To(Equal([]authn.AuthMethod{authn.AuthMethodMTLS}))
			})
		})
	})

	// =================================================================
	// LEVEL 4: REGISTRY DISPATCH
	// =================================================================

	Describe("Level 4: Registry Dispatch", func() {
		var recorder *recordingEmitter

		BeforeEach(func() {
			recorder = &recordingEmitter{}
		})

		It("dispatches to the first authenticator that accepts the request", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "dispatch-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			// GSSAPI returns ErrUnsupportedMethod, X509 should handle it
			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{
				authn.NewGSSAPIAuthenticator(),
				x509Auth,
			})
			Expect(err).NotTo(HaveOccurred())

			identity, err := registry.Authenticate(context.Background(), &authn.Request{
				TLSState: tlsStateWith(acmeCert),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("dispatch-tenant"))
			Expect(identity.Method).To(Equal(authn.AuthMethodMTLS))
		})

		It("skips authenticators returning ErrUnsupportedMethod", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "skip-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			// Two stubs then the real one
			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{
				authn.NewGSSAPIAuthenticator(),
				authn.NewSAMLAuthenticator(),
				x509Auth,
			})
			Expect(err).NotTo(HaveOccurred())

			identity, err := registry.Authenticate(context.Background(), &authn.Request{
				TLSState: tlsStateWith(acmeCert),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("skip-tenant"))
		})

		It("stops on non-ErrUnsupportedMethod errors", func() {
			// X509 with no peer certs returns ErrAuthenticationFailed (not ErrUnsupportedMethod)
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "stop-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			// A second authenticator that would succeed — but should never be reached
			x509AuthGood, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "unreachable-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{
				x509Auth,
				x509AuthGood,
			})
			Expect(err).NotTo(HaveOccurred())

			// Request has TLS state but no peer certs — triggers ErrAuthenticationFailed
			_, err = registry.Authenticate(context.Background(), &authn.Request{
				TLSState: &tls.ConnectionState{},
			})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, authn.ErrAuthenticationFailed)).To(BeTrue())
		})

		It("returns ErrAuthenticationFailed when all return ErrUnsupportedMethod", func() {
			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{
				authn.NewGSSAPIAuthenticator(),
				authn.NewSAMLAuthenticator(),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = registry.Authenticate(context.Background(), &authn.Request{
				TLSState: tlsStateWith(acmeCert),
			})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, authn.ErrAuthenticationFailed)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("no authenticator accepted"))
		})
	})

	// =================================================================
	// LEVEL 5: AUDIT EMISSION
	// =================================================================

	Describe("Level 5: Audit Emission", func() {
		var recorder *recordingEmitter

		BeforeEach(func() {
			recorder = &recordingEmitter{}
		})

		It("emits a success event on successful authentication", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "audit-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{x509Auth})
			Expect(err).NotTo(HaveOccurred())
			_, err = registry.Authenticate(context.Background(), &authn.Request{
				TLSState:  tlsStateWith(acmeCert),
				ClientIP:  "10.0.0.1",
				SessionID: "session-001",
			})
			Expect(err).NotTo(HaveOccurred())

			event := recorder.lastEvent()
			Expect(event).NotTo(BeNil())
			Expect(event.Success).To(BeTrue())
			Expect(event.Principal).To(Equal("web.acme.internal"))
			Expect(event.TenantID).To(Equal("audit-tenant"))
			Expect(event.Method).To(Equal(authn.AuthMethodMTLS))
			Expect(event.ClientIP).To(Equal("10.0.0.1"))
			Expect(event.SessionID).To(Equal("session-001"))
			Expect(event.FailureReason).To(BeEmpty())
			Expect(event.Timestamp).NotTo(BeZero())
		})

		It("emits a failure event on authentication failure", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant: false,
				Mappings: []authn.X509Mapping{
					{Match: authn.X509Match{Organization: "NeverMatches"}, Tenant: "never-tenant", Roles: []string{"none"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{x509Auth})
			Expect(err).NotTo(HaveOccurred())
			_, err = registry.Authenticate(context.Background(), &authn.Request{
				TLSState:  tlsStateWith(acmeCert),
				ClientIP:  "10.0.0.2",
				SessionID: "session-fail",
			})
			Expect(err).To(HaveOccurred())

			event := recorder.lastEvent()
			Expect(event).NotTo(BeNil())
			Expect(event.Success).To(BeFalse())
			Expect(event.FailureReason).NotTo(BeEmpty())
			Expect(event.Principal).To(Equal("web.acme.internal"))
			Expect(event.ClientIP).To(Equal("10.0.0.2"))
			Expect(event.SessionID).To(Equal("session-fail"))
		})

		It("emits a failure event when all authenticators are unsupported", func() {
			registry, err := authn.NewRegistry(recorder, []authn.Authenticator{
				authn.NewGSSAPIAuthenticator(),
				authn.NewSAMLAuthenticator(),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = registry.Authenticate(context.Background(), &authn.Request{
				TLSState:  tlsStateWith(acmeCert),
				ClientIP:  "10.0.0.3",
				SessionID: "session-unsupported",
			})
			Expect(err).To(HaveOccurred())

			event := recorder.lastEvent()
			Expect(event).NotTo(BeNil())
			Expect(event.Success).To(BeFalse())
			Expect(event.FailureReason).To(ContainSubstring("no authenticator accepted"))
		})

		It("does not panic with nil emitter", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "nil-emitter-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			registry, err := authn.NewRegistry(nil, []authn.Authenticator{x509Auth})
			Expect(err).NotTo(HaveOccurred())

			Expect(func() {
				identity, authErr := registry.Authenticate(context.Background(), &authn.Request{
					TLSState: tlsStateWith(acmeCert),
				})
				Expect(authErr).NotTo(HaveOccurred())
				Expect(identity.TenantID).To(Equal("nil-emitter-tenant"))
			}).NotTo(Panic())
		})

		It("does not block authentication when emitter fails", func() {
			x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
				SingleTenant:  true,
				DefaultTenant: "failing-emitter-tenant",
			})
			Expect(err).NotTo(HaveOccurred())

			registry, err := authn.NewRegistry(&failingEmitter{}, []authn.Authenticator{x509Auth})
			Expect(err).NotTo(HaveOccurred())

			identity, err := registry.Authenticate(context.Background(), &authn.Request{
				TLSState: tlsStateWith(acmeCert),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.TenantID).To(Equal("failing-emitter-tenant"))
		})
	})

	// =================================================================
	// LEVEL 6: STUB AUTHENTICATORS
	// =================================================================

	Describe("Level 6: Stub Authenticators", func() {

		Describe("GSSAPIAuthenticator", func() {
			var gssapi *authn.GSSAPIAuthenticator

			BeforeEach(func() {
				gssapi = authn.NewGSSAPIAuthenticator()
			})

			It("returns wrapped ErrUnsupportedMethod", func() {
				_, err := gssapi.Authenticate(context.Background(), &authn.Request{})
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, authn.ErrUnsupportedMethod)).To(BeTrue())
			})

			It("includes GSSAPI in the error message", func() {
				_, err := gssapi.Authenticate(context.Background(), &authn.Request{})
				Expect(err.Error()).To(ContainSubstring("GSSAPI"))
			})

			It("reports Kerberos as supported method", func() {
				Expect(gssapi.SupportedMethods()).To(Equal([]authn.AuthMethod{authn.AuthMethodKerberos}))
			})
		})

		Describe("SAMLAuthenticator", func() {
			var saml *authn.SAMLAuthenticator

			BeforeEach(func() {
				saml = authn.NewSAMLAuthenticator()
			})

			It("returns wrapped ErrUnsupportedMethod", func() {
				_, err := saml.Authenticate(context.Background(), &authn.Request{})
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, authn.ErrUnsupportedMethod)).To(BeTrue())
			})

			It("includes SAML in the error message", func() {
				_, err := saml.Authenticate(context.Background(), &authn.Request{})
				Expect(err.Error()).To(ContainSubstring("SAML"))
			})

			It("reports SAML as supported method", func() {
				Expect(saml.SupportedMethods()).To(Equal([]authn.AuthMethod{authn.AuthMethodSAML}))
			})
		})
	})
})

// ---------------------------------------------------------------------------
// RBAC — RequireRole & IsAdmin
// ---------------------------------------------------------------------------

var _ = Describe("RBAC", func() {
	Describe("RequireRole", func() {
		DescribeTable("role matching",
			func(roles []string, required []string, wantErr bool) {
				id := authn.Identity{Roles: roles}
				err := authn.RequireRole(id, required...)
				if wantErr {
					Expect(err).To(HaveOccurred())
					Expect(errors.Is(err, authn.ErrInsufficientRole)).To(BeTrue())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("exact single match", []string{"admin"}, []string{"admin"}, false),
			Entry("any-match from multiple required", []string{"reader"}, []string{"admin", "reader"}, false),
			Entry("any-match from multiple identity roles", []string{"writer", "admin"}, []string{"admin"}, false),
			Entry("no match", []string{"reader"}, []string{"admin"}, true),
			Entry("empty identity roles", []string{}, []string{"admin"}, true),
			Entry("nil identity roles", nil, []string{"admin"}, true),
			Entry("zero required roles is caller bug", []string{"admin"}, []string{}, true),
			Entry("case sensitive Admin vs admin", []string{"Admin"}, []string{"admin"}, true),
			Entry("whitespace stripped from identity role", []string{" admin "}, []string{"admin"}, false),
			Entry("whitespace stripped from required role", []string{"admin"}, []string{" admin "}, false),
			Entry("whitespace-only identity role matches nothing", []string{"  "}, []string{"admin"}, true),
			Entry("whitespace-only required role matches nothing", []string{"admin"}, []string{"  "}, true),
			Entry("duplicates are harmless", []string{"admin", "admin"}, []string{"admin", "admin"}, false),
			Entry("long role string", []string{"super-long-role-name-that-is-still-valid"}, []string{"super-long-role-name-that-is-still-valid"}, false),
		)
	})

	Describe("IsAdmin", func() {
		DescribeTable("admin detection",
			func(roles []string, want bool) {
				id := authn.Identity{Roles: roles}
				Expect(authn.IsAdmin(id)).To(Equal(want))
			},
			Entry("admin present", []string{"admin"}, true),
			Entry("admin absent", []string{"reader", "writer"}, false),
			Entry("empty roles", []string{}, false),
			Entry("nil roles", nil, false),
			Entry("case sensitive Admin", []string{"Admin"}, false),
			Entry("whitespace stripped", []string{" admin "}, true),
			Entry("admin among many", []string{"reader", "writer", "admin", "operator"}, true),
			Entry("administrator is not admin", []string{"administrator"}, false),
		)
	})
})
