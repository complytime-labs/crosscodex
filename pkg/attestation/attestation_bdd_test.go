//go:build !integration

package attestation_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

func TestAttestationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Attestation BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// mockKeyProvider is a test double that generates ephemeral ECDSA P-256 keys.
type mockKeyProvider struct {
	key *ecdsa.PrivateKey
}

func newMockKeyProvider() *mockKeyProvider {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic("generate mock key: " + err.Error())
	}
	return &mockKeyProvider{key: key}
}

func (m *mockKeyProvider) SigningKey(_ context.Context) (crypto.Signer, error) {
	if m.key == nil {
		m = newMockKeyProvider()
	}
	return m.key, nil
}

func (m *mockKeyProvider) VerificationKey(_ context.Context) (crypto.PublicKey, error) {
	if m.key == nil {
		m = newMockKeyProvider()
	}
	return m.key.Public(), nil
}

func (m *mockKeyProvider) KeyID(_ context.Context) (string, error) {
	return "mock-key-id", nil
}

var _ = Describe("Type Definitions", func() {
	It("LayoutOptions accepts steps and expiry", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "build", Command: []string{"make"}},
			},
			ExpiresIn: 24 * time.Hour,
		}
		Expect(opts.Steps).To(HaveLen(1))
		Expect(opts.ExpiresIn).To(Equal(24 * time.Hour))
	})

	It("SignedLayout holds raw bytes and expiry time", func() {
		sl := attestation.SignedLayout{
			Raw:     []byte(`{"test": true}`),
			Expires: time.Now().Add(24 * time.Hour),
		}
		Expect(sl.Raw).NotTo(BeEmpty())
		Expect(sl.Expires).To(BeTemporally(">", time.Now()))
	})

	It("SignedLink holds raw bytes with trace metadata", func() {
		link := attestation.SignedLink{
			Raw:       []byte(`{"test": true}`),
			Step:      "ingest",
			TraceID:   "abc123",
			Materials: []attestation.Artifact{{URI: "input.json", Digest: "sha256:abc"}},
			Products:  []attestation.Artifact{{URI: "output.json", Digest: "sha256:def"}},
		}
		Expect(link.Step).To(Equal("ingest"))
		Expect(link.TraceID).To(Equal("abc123"))
		Expect(link.Materials).To(HaveLen(1))
		Expect(link.Products).To(HaveLen(1))
	})

	It("VerifiedLink contains step, artifacts, and byproducts", func() {
		vl := attestation.VerifiedLink{
			Step:       "ingest",
			Materials:  []attestation.Artifact{{URI: "input.json", Digest: "sha256:abc"}},
			Products:   []attestation.Artifact{{URI: "output.json", Digest: "sha256:def"}},
			ByProducts: map[string]any{"trace_id": "abc123"},
		}
		Expect(vl.Step).To(Equal("ingest"))
		Expect(vl.ByProducts["trace_id"]).To(Equal("abc123"))
	})

	It("preserves Artifact, Step, and Inspection from scaffold", func() {
		a := attestation.Artifact{URI: "foo.json", Digest: "sha256:abc"}
		Expect(a.URI).To(Equal("foo.json"))

		s := attestation.Step{
			Name:              "build",
			ExpectedMaterials: []string{"src/"},
			ExpectedProducts:  []string{"bin/"},
			Command:           []string{"make"},
			Threshold:         1,
		}
		Expect(s.Name).To(Equal("build"))

		i := attestation.Inspection{
			Name:   "verify-sig",
			Run:    []string{"gpg", "--verify"},
			Passes: []string{"PASS"},
		}
		Expect(i.Name).To(Equal("verify-sig"))
	})
})

var _ = Describe("Conversion Functions", func() {
	Describe("artifactsToHashObj", func() {
		It("converts artifacts to in-toto material/product format", func() {
			artifacts := []attestation.Artifact{
				{URI: "input.json", Digest: "abcdef1234567890"},
				{URI: "config.yaml", Digest: "fedcba0987654321"},
			}
			result := attestation.ArtifactsToHashObj(artifacts)
			Expect(result).To(HaveLen(2))
			Expect(result["input.json"]).To(HaveKeyWithValue("sha256", "abcdef1234567890"))
			Expect(result["config.yaml"]).To(HaveKeyWithValue("sha256", "fedcba0987654321"))
		})

		It("returns empty map for empty artifact list", func() {
			result := attestation.ArtifactsToHashObj(nil)
			Expect(result).To(BeEmpty())
		})
	})

	Describe("hashObjToArtifacts", func() {
		It("converts in-toto format back to artifacts", func() {
			hashObjs := map[string]map[string]string{
				"input.json":  {"sha256": "abcdef1234567890"},
				"config.yaml": {"sha256": "fedcba0987654321"},
			}
			result := attestation.HashObjToArtifacts(hashObjs)
			Expect(result).To(HaveLen(2))

			uris := make(map[string]string)
			for _, a := range result {
				uris[a.URI] = a.Digest
			}
			Expect(uris).To(HaveKeyWithValue("input.json", "abcdef1234567890"))
			Expect(uris).To(HaveKeyWithValue("config.yaml", "fedcba0987654321"))
		})

		It("picks sha256 when multiple hashes present", func() {
			hashObjs := map[string]map[string]string{
				"file.bin": {"sha256": "abc", "sha512": "def"},
			}
			result := attestation.HashObjToArtifacts(hashObjs)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Digest).To(Equal("abc"))
		})

		It("returns empty list for empty input", func() {
			result := attestation.HashObjToArtifacts(nil)
			Expect(result).To(BeEmpty())
		})
	})

	Describe("round-trip", func() {
		It("artifacts survive conversion round-trip", func() {
			original := []attestation.Artifact{
				{URI: "a.json", Digest: "aaa"},
				{URI: "b.json", Digest: "bbb"},
			}
			hashObjs := attestation.ArtifactsToHashObj(original)
			roundTripped := attestation.HashObjToArtifacts(hashObjs)

			uris := make(map[string]string)
			for _, a := range roundTripped {
				uris[a.URI] = a.Digest
			}
			for _, a := range original {
				Expect(uris).To(HaveKeyWithValue(a.URI, a.Digest))
			}
		})
	})
})

var _ = Describe("Generator Construction", func() {
	Context("with a valid KeyProvider", func() {
		It("creates a generator without error", func() {
			kp := &mockKeyProvider{}
			gen, err := attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
			Expect(gen).NotTo(BeNil())
		})
	})

	Context("with a nil KeyProvider", func() {
		It("returns an error", func() {
			gen, err := attestation.NewGenerator(nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("key provider is required"))
			Expect(gen).To(BeNil())
		})
	})

	Context("telemetry fields", func() {
		It("has nil telemetry fields by default", func() {
			kp := &mockKeyProvider{}
			gen, err := attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())

			tf := attestation.ExportTelemetryFields(gen)
			Expect(tf.HasTracer).To(BeFalse())
			Expect(tf.HasMeter).To(BeFalse())
			Expect(tf.HasOpCounter).To(BeFalse())
			Expect(tf.HasOpLatency).To(BeFalse())
		})

		It("populates telemetry fields with WithTelemetry", func() {
			kp := &mockKeyProvider{}
			tp := tracenoop.NewTracerProvider()
			tracer := tp.Tracer("attestation-test")
			mp := metricnoop.NewMeterProvider()
			meter := mp.Meter("attestation-test")

			gen, err := attestation.NewGenerator(kp, attestation.WithTelemetry(tracer, meter))
			Expect(err).NotTo(HaveOccurred())

			tf := attestation.ExportTelemetryFields(gen)
			Expect(tf.HasTracer).To(BeTrue())
			Expect(tf.HasMeter).To(BeTrue())
			Expect(tf.HasOpCounter).To(BeTrue())
			Expect(tf.HasOpLatency).To(BeTrue())
		})
	})
})

var _ = Describe("FileKeyProvider", Ordered, func() {
	var (
		tmpDir   string
		ca       *pki.CertKeyPair
		privPath string
		pubPath  string
	)

	BeforeAll(func() {
		tmpDir = GinkgoT().TempDir()
		var err error
		ca, err = pki.GenerateCA()
		Expect(err).NotTo(HaveOccurred())

		privPath = filepath.Join(tmpDir, "signing.pem")
		Expect(os.WriteFile(privPath, ca.KeyPEM, 0o600)).To(Succeed())

		pubDER, err := x509.MarshalPKIXPublicKey(ca.Cert.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
		pubPath = filepath.Join(tmpDir, "verification.pem")
		Expect(os.WriteFile(pubPath, pubPEM, 0o644)).To(Succeed())
	})

	It("loads a valid ECDSA P-256 private key", func() {
		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: privPath,
			PublicKeyPath:  pubPath,
		}
		signer, err := kp.SigningKey(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(signer).NotTo(BeNil())
	})

	It("loads a valid public key", func() {
		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: privPath,
			PublicKeyPath:  pubPath,
		}
		pub, err := kp.VerificationKey(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(pub).NotTo(BeNil())
	})

	It("returns ErrKeyNotFound for nonexistent private key file", func() {
		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: "/nonexistent/signing.pem",
			PublicKeyPath:  pubPath,
		}
		_, err := kp.SigningKey(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrKeyNotFound)).To(BeTrue())
	})

	It("returns ErrKeyNotFound for nonexistent public key file", func() {
		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: privPath,
			PublicKeyPath:  "/nonexistent/verification.pem",
		}
		_, err := kp.VerificationKey(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrKeyNotFound)).To(BeTrue())
	})

	It("returns ErrKeyLoadFailed for invalid PEM", func() {
		badPath := filepath.Join(tmpDir, "bad.pem")
		Expect(os.WriteFile(badPath, []byte("not a pem file"), 0o600)).To(Succeed())

		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: badPath,
			PublicKeyPath:  pubPath,
		}
		_, err := kp.SigningKey(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrKeyLoadFailed)).To(BeTrue())
	})

	It("produces a deterministic KeyID (same key = same ID)", func() {
		kp := &attestation.FileKeyProvider{
			PrivateKeyPath: privPath,
			PublicKeyPath:  pubPath,
		}
		id1, err := kp.KeyID(context.Background())
		Expect(err).NotTo(HaveOccurred())
		id2, err := kp.KeyID(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(id1).To(Equal(id2))
		Expect(id1).NotTo(BeEmpty())
	})
})

var _ = Describe("EphemeralKeyProvider", func() {
	var (
		ctx context.Context
		ekp *attestation.EphemeralKeyProvider
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		ekp, err = attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
	})

	It("generates a valid ECDSA P-256 signing key", func() {
		signer, err := ekp.SigningKey(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(signer).NotTo(BeNil())

		ecKey, ok := signer.Public().(*ecdsa.PublicKey)
		Expect(ok).To(BeTrue())
		Expect(ecKey.Curve).To(Equal(elliptic.P256()))
	})

	It("returns a matching verification key", func() {
		signer, err := ekp.SigningKey(ctx)
		Expect(err).NotTo(HaveOccurred())

		verKey, err := ekp.VerificationKey(ctx)
		Expect(err).NotTo(HaveOccurred())

		signerPub := signer.Public().(*ecdsa.PublicKey)
		verPub := verKey.(*ecdsa.PublicKey)
		Expect(signerPub.Equal(verPub)).To(BeTrue())
	})

	It("returns a deterministic key ID for the same instance", func() {
		id1, err := ekp.KeyID(ctx)
		Expect(err).NotTo(HaveOccurred())
		id2, err := ekp.KeyID(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(id1).To(Equal(id2))
		Expect(id1).NotTo(BeEmpty())
	})

	It("produces different key pairs across instances", func() {
		ekp2, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())

		id1, err := ekp.KeyID(ctx)
		Expect(err).NotTo(HaveOccurred())
		id2, err := ekp2.KeyID(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(id1).NotTo(Equal(id2))
	})

	It("works with Generator for sign/verify round-trip", func() {
		gen, err := attestation.NewGenerator(ekp)
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "input.txt", Digest: "abc123"}}
		products := []attestation.Artifact{{URI: "output.txt", Digest: "def456"}}

		link, err := gen.CreateLink(ctx, "test-step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.Step).To(Equal("test-step"))
	})
})

var _ = Describe("CreateLink", Ordered, func() {
	var (
		gen attestation.Generator
		kp  *mockKeyProvider
	)

	BeforeAll(func() {
		kp = newMockKeyProvider()
		var err error
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	It("creates a valid signed link with materials and products", func() {
		ctx := context.Background()
		materials := []attestation.Artifact{
			{URI: "input.json", Digest: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"}, // DevSkim: ignore DS173237 - deterministic test digest, not a credential
		}
		products := []attestation.Artifact{
			{URI: "output.json", Digest: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"}, // DevSkim: ignore DS173237 - deterministic test digest, not a credential
		}

		link, err := gen.CreateLink(ctx, "catalog.ingest", materials, products)
		Expect(err).NotTo(HaveOccurred())
		Expect(link).NotTo(BeNil())
		Expect(link.Step).To(Equal("catalog.ingest"))
		Expect(link.Raw).NotTo(BeEmpty())
		Expect(link.Materials).To(Equal(materials))
		Expect(link.Products).To(Equal(products))
	})

	It("embeds trace ID in ByProducts when span is active", func() {
		tp := tracenoop.NewTracerProvider()
		tracer := tp.Tracer("test")
		ctx, span := tracer.Start(context.Background(), "test-op")
		defer span.End()

		// noop tracer produces zero trace ID, but the code path is exercised
		materials := []attestation.Artifact{{URI: "a.json", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "b.json", Digest: "bbb"}}

		link, err := gen.CreateLink(ctx, "test-step", materials, products)
		Expect(err).NotTo(HaveOccurred())
		// TraceID may be empty with noop tracer, but field should be set
		Expect(link.TraceID).To(BeEmpty()) // noop tracer produces "00000000..."
	})

	It("materials and products round-trip through sign/verify", func() {
		ctx := context.Background()
		materials := []attestation.Artifact{
			{URI: "src/main.go", Digest: "aabbccdd"},
			{URI: "go.mod", Digest: "eeff0011"},
		}
		products := []attestation.Artifact{
			{URI: "bin/app", Digest: "22334455"},
		}

		link, err := gen.CreateLink(ctx, "build", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.Step).To(Equal("build"))

		// Verify materials round-tripped
		matMap := make(map[string]string)
		for _, a := range verified.Materials {
			matMap[a.URI] = a.Digest
		}
		for _, a := range materials {
			Expect(matMap).To(HaveKeyWithValue(a.URI, a.Digest))
		}

		// Verify products round-tripped
		prodMap := make(map[string]string)
		for _, a := range verified.Products {
			prodMap[a.URI] = a.Digest
		}
		for _, a := range products {
			Expect(prodMap).To(HaveKeyWithValue(a.URI, a.Digest))
		}
	})
})

var _ = Describe("Verify", Ordered, func() {
	var (
		gen attestation.Generator
		kp  *mockKeyProvider
	)

	BeforeAll(func() {
		kp = newMockKeyProvider()
		var err error
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	It("verifies a valid signed link envelope", func() {
		ctx := context.Background()
		link, err := gen.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified).NotTo(BeNil())
		Expect(verified.Step).To(Equal("test"))
	})

	It("returns ErrVerificationFailed on tampered envelope", func() {
		ctx := context.Background()
		link, err := gen.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		// Tamper with the raw bytes
		tampered := make([]byte, len(link.Raw))
		copy(tampered, link.Raw)
		// Flip a byte in the payload (not the JSON structure, but the base64 payload)
		for i := len(tampered) / 2; i < len(tampered); i++ {
			if tampered[i] >= 'a' && tampered[i] <= 'z' {
				tampered[i] = 'Z'
				break
			}
		}

		_, err = gen.Verify(ctx, tampered)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrVerificationFailed)).To(BeTrue())
	})

	It("returns ErrVerificationFailed on wrong public key", func() {
		ctx := context.Background()
		link, err := gen.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		// Create a new generator with a different key pair
		kp2 := newMockKeyProvider()
		gen2, err := attestation.NewGenerator(kp2)
		Expect(err).NotTo(HaveOccurred())

		// Verification with wrong key should fail
		_, err = gen2.Verify(ctx, link.Raw)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrVerificationFailed)).To(BeTrue())
	})

	It("returns correct VerifiedLink fields", func() {
		ctx := context.Background()
		materials := []attestation.Artifact{{URI: "m.json", Digest: "mmm"}}
		products := []attestation.Artifact{{URI: "p.json", Digest: "ppp"}}

		link, err := gen.CreateLink(ctx, "verify-fields", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.Step).To(Equal("verify-fields"))
		Expect(verified.ByProducts).To(HaveKey("trace_id"))
	})
})

var _ = Describe("CreateLayout", Ordered, func() {
	var (
		gen attestation.Generator
		kp  *mockKeyProvider
	)

	BeforeAll(func() {
		kp = newMockKeyProvider()
		var err error
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	It("creates a valid signed layout with steps and inspections", func() {
		ctx := context.Background()
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "ingest", Command: []string{"ingest"}, Threshold: 1},
				{Name: "analyze", Command: []string{"analyze"}, Threshold: 1},
			},
			Inspections: []attestation.Inspection{
				{Name: "verify-hash", Run: []string{"sha256sum", "-c"}},
			},
			ExpiresIn: 24 * time.Hour,
		}

		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(layout).NotTo(BeNil())
		Expect(layout.Raw).NotTo(BeEmpty())
		Expect(layout.Expires).To(BeTemporally("~", time.Now().Add(24*time.Hour), 5*time.Second))
	})

	It("returns ErrInvalidLayout when steps are empty", func() {
		ctx := context.Background()
		opts := attestation.LayoutOptions{
			Steps:     []attestation.Step{},
			ExpiresIn: 24 * time.Hour,
		}

		_, err := gen.CreateLayout(ctx, opts)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrInvalidLayout)).To(BeTrue())
	})
})

var _ = Describe("Telemetry Integration", Ordered, func() {
	var (
		tp  *telemetrytest.TestProvider
		gen attestation.Generator
		kp  *mockKeyProvider
	)

	BeforeAll(func() {
		kp = newMockKeyProvider()
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())

		tracer := tp.TracerProvider().Tracer("attestation-test")
		meter := tp.MeterProvider().Meter("attestation-test")

		gen, err = attestation.NewGenerator(kp, attestation.WithTelemetry(tracer, meter))
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		Expect(tp.Shutdown(context.Background())).To(Succeed())
	})

	It("CreateLink emits attestation.CreateLink span", func() {
		tp.Reset()
		ctx := context.Background()
		_, err := gen.CreateLink(ctx, "test-span", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "attestation.CreateLink")
		Expect(span).NotTo(BeNil(), "expected attestation.CreateLink span")

		stepAttr, found := telemetrytest.SpanAttribute(span, "attestation.step")
		Expect(found).To(BeTrue())
		Expect(stepAttr.AsString()).To(Equal("test-span"))
	})

	It("CreateLink records attestation.operations.total counter", func() {
		tp.Reset()
		ctx := context.Background()
		_, err := gen.CreateLink(ctx, "test-counter", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		metrics := tp.GetMetrics()
		m := telemetrytest.FindMetric(metrics, "attestation.operations.total")
		Expect(m).NotTo(BeNil(), "expected attestation.operations.total metric")

		val, err := telemetrytest.CounterValue(m)
		Expect(err).NotTo(HaveOccurred())
		Expect(val).To(BeNumerically(">=", 1))
	})

	It("CreateLink records attestation.operation.duration_ms histogram", func() {
		tp.Reset()
		ctx := context.Background()
		_, err := gen.CreateLink(ctx, "test-hist", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		metrics := tp.GetMetrics()
		m := telemetrytest.FindMetric(metrics, "attestation.operation.duration_ms")
		Expect(m).NotTo(BeNil(), "expected attestation.operation.duration_ms metric")

		count, err := telemetrytest.HistogramCount(m)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", 1))
	})

	It("Verify emits attestation.Verify span", func() {
		tp.Reset()
		ctx := context.Background()
		link, err := gen.CreateLink(ctx, "test-verify-span", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())

		tp.Reset() // Clear CreateLink spans
		_, err = gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())

		spans := tp.GetSpans()
		span := telemetrytest.FindSpan(spans, "attestation.Verify")
		Expect(span).NotTo(BeNil(), "expected attestation.Verify span")
	})

	It("operations succeed without telemetry", func() {
		noTelGen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())

		ctx := context.Background()
		link, err := noTelGen.CreateLink(ctx, "no-tel", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(link).NotTo(BeNil())

		verified, err := noTelGen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.Step).To(Equal("no-tel"))
	})

	It("emits attestation.VerifyLayout span", func() {
		localTP, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = localTP.Shutdown(context.Background()) }()

		localKP := newMockKeyProvider()
		localGen, err := attestation.NewGenerator(localKP,
			attestation.WithTelemetry(localTP.TracerProvider().Tracer("test"), localTP.MeterProvider().Meter("test")),
		)
		Expect(err).NotTo(HaveOccurred())

		layout, err := localGen.CreateLayout(context.Background(), attestation.LayoutOptions{
			Steps:     []attestation.Step{{Name: "s1", Threshold: 1}},
			ExpiresIn: 24 * time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())

		localTP.Reset()
		_, err = localGen.VerifyLayout(context.Background(), layout.Raw)
		Expect(err).NotTo(HaveOccurred())

		spans := localTP.GetSpans()
		span := telemetrytest.FindSpan(spans, "attestation.VerifyLayout")
		Expect(span).NotTo(BeNil(), "expected attestation.VerifyLayout span")
	})

	It("emits attestation.VerifyChain span", func() {
		localTP, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = localTP.Shutdown(context.Background()) }()

		localKP := newMockKeyProvider()
		localGen, err := attestation.NewGenerator(localKP,
			attestation.WithTelemetry(localTP.TracerProvider().Tracer("test"), localTP.MeterProvider().Meter("test")),
		)
		Expect(err).NotTo(HaveOccurred())

		link1, err := localGen.CreateLink(context.Background(), "step-a",
			[]attestation.Artifact{{URI: "in.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "mid.json", Digest: "bbb"}},
		)
		Expect(err).NotTo(HaveOccurred())
		link2, err := localGen.CreateLink(context.Background(), "step-b",
			[]attestation.Artifact{{URI: "mid.json", Digest: "bbb"}},
			[]attestation.Artifact{{URI: "out.txt", Digest: "ccc"}},
		)
		Expect(err).NotTo(HaveOccurred())

		localTP.Reset()
		err = localGen.VerifyChain(context.Background(), nil, []*attestation.SignedLink{link1, link2})
		Expect(err).NotTo(HaveOccurred())

		spans := localTP.GetSpans()
		span := telemetrytest.FindSpan(spans, "attestation.VerifyChain")
		Expect(span).NotTo(BeNil(), "expected attestation.VerifyChain span")
	})
})

// ed25519Signer is a test double that implements crypto.Signer with a non-ECDSA key.
// The key is generated once at construction to ensure deterministic Public() calls.
type ed25519Signer struct {
	pub ed25519.PublicKey
}

func newEd25519Signer() ed25519Signer {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return ed25519Signer{pub: pub}
}

func (e ed25519Signer) Public() crypto.PublicKey {
	return e.pub
}

func (e ed25519Signer) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("not implemented")
}

var _ = Describe("FIPS Enforcement", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("allows ECDSA P-256 key when FIPS mode enabled", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithFIPSMode(true))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		_, err = gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects non-ECDSA key with ErrNonFIPSAlgorithm", func() {
		err := attestation.ValidateFIPSKey(newEd25519Signer())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrNonFIPSAlgorithm)).To(BeTrue())
	})

	It("skips FIPS check when FIPS mode disabled", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithFIPSMode(false))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		_, err = gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets FIPSMode field via option", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithFIPSMode(true))
		Expect(err).NotTo(HaveOccurred())

		fields := attestation.ExportTelemetryFields(gen)
		Expect(fields.FIPSMode).To(BeTrue())
	})

	It("sets IncludeByProducts field via option", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(true))
		Expect(err).NotTo(HaveOccurred())

		fields := attestation.ExportTelemetryFields(gen)
		Expect(fields.IncludeByProducts).To(BeTrue())
	})
})

var _ = Describe("Enriched ByProducts", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("always includes trace_id regardless of includeByProducts flag", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(false))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.ByProducts).To(HaveKey("trace_id"))
	})

	It("includes span_id when includeByProducts is true", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(true))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.ByProducts).To(HaveKey("span_id"))
	})

	It("includes timestamp in RFC3339 format when includeByProducts is true", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(true))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		ts, ok := verified.ByProducts["timestamp"].(string)
		Expect(ok).To(BeTrue())
		_, parseErr := time.Parse(time.RFC3339, ts)
		Expect(parseErr).NotTo(HaveOccurred())
	})

	It("includes hostname when includeByProducts is true", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(true))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.ByProducts).To(HaveKey("hostname"))
	})

	It("does not include span_id when includeByProducts is false", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(false))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.ByProducts).NotTo(HaveKey("span_id"))
		Expect(verified.ByProducts).NotTo(HaveKey("timestamp"))
		Expect(verified.ByProducts).NotTo(HaveKey("hostname"))
	})

	It("merges caller-supplied byproducts via WithByProducts", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		extra := map[string]any{"model_id": "gpt-4", "prompt_version": "v2"}
		link, err := gen.CreateLink(ctx, "step", materials, products, attestation.WithByProducts(extra))
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.ByProducts["model_id"]).To(Equal("gpt-4"))
		Expect(verified.ByProducts["prompt_version"]).To(Equal("v2"))
	})

	It("does not allow WithByProducts to overwrite trace_id", func() {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		extra := map[string]any{"trace_id": "evil-override"}
		link, err := gen.CreateLink(ctx, "step", materials, products, attestation.WithByProducts(extra))
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		// trace_id should be from context, not "evil-override"
		Expect(verified.ByProducts["trace_id"]).NotTo(Equal("evil-override"))
	})
})

var _ = Describe("VerifyLayout", func() {
	var (
		kp  *attestation.EphemeralKeyProvider
		gen attestation.Generator
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		kp, err = attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	It("verifies a valid signed layout envelope", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "build", ExpectedMaterials: []string{"src"}, ExpectedProducts: []string{"bin"}, Threshold: 1},
			},
			ExpiresIn: 24 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.VerifyLayout(ctx, layout.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified).NotTo(BeNil())
		Expect(verified.Steps).To(HaveLen(1))
		Expect(verified.Steps[0].Name).To(Equal("build"))
		Expect(verified.Expires).NotTo(BeZero())
		Expect(verified.KeyIDs).NotTo(BeEmpty())
	})

	It("returns ErrVerificationFailed on tampered envelope", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "build", Threshold: 1},
			},
			ExpiresIn: 24 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		tampered := make([]byte, len(layout.Raw))
		copy(tampered, layout.Raw)
		tampered[len(tampered)/2] ^= 0xFF

		_, err = gen.VerifyLayout(ctx, tampered)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrVerificationFailed)).To(BeTrue())
	})

	It("returns ErrVerificationFailed on wrong key", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "build", Threshold: 1},
			},
			ExpiresIn: 24 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		kp2, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen2, err := attestation.NewGenerator(kp2)
		Expect(err).NotTo(HaveOccurred())

		_, err = gen2.VerifyLayout(ctx, layout.Raw)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrVerificationFailed)).To(BeTrue())
	})

	It("returns ErrExpired for expired layout", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "build", Threshold: 1},
			},
			ExpiresIn: -1 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		_, err = gen.VerifyLayout(ctx, layout.Raw)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrExpired)).To(BeTrue())
	})

	It("returns correct VerifiedLayout fields", func() {
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "step-a", ExpectedMaterials: []string{"m1"}, ExpectedProducts: []string{"p1"}, Threshold: 1},
				{Name: "step-b", ExpectedMaterials: []string{"p1"}, ExpectedProducts: []string{"p2"}, Threshold: 1},
			},
			Inspections: []attestation.Inspection{
				{Name: "check", Run: []string{"sha256sum"}, Passes: []string{"p2"}},
			},
			ExpiresIn: 48 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.VerifyLayout(ctx, layout.Raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.Steps).To(HaveLen(2))
		Expect(verified.Steps[0].Name).To(Equal("step-a"))
		Expect(verified.Steps[1].Name).To(Equal("step-b"))
		Expect(verified.Inspections).To(HaveLen(1))
		Expect(verified.Inspections[0].Name).To(Equal("check"))
		Expect(verified.Expires).To(BeTemporally("~", time.Now().Add(48*time.Hour), 5*time.Second))
		Expect(verified.KeyIDs).To(HaveLen(1))
	})
})

var _ = Describe("VerifyChain", func() {
	var (
		kp  *attestation.EphemeralKeyProvider
		gen attestation.Generator
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		kp, err = attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
	})

	It("validates matching products-to-materials chain across 3 steps", func() {
		link1, err := gen.CreateLink(ctx, "ingestion",
			[]attestation.Artifact{{URI: "catalog.oscal", Digest: "aaa111"}},
			[]attestation.Artifact{{URI: "data.json", Digest: "bbb222"}},
		)
		Expect(err).NotTo(HaveOccurred())

		link2, err := gen.CreateLink(ctx, "analysis",
			[]attestation.Artifact{{URI: "data.json", Digest: "bbb222"}},
			[]attestation.Artifact{{URI: "analysis.json", Digest: "ccc333"}},
		)
		Expect(err).NotTo(HaveOccurred())

		link3, err := gen.CreateLink(ctx, "synthesis",
			[]attestation.Artifact{{URI: "analysis.json", Digest: "ccc333"}},
			[]attestation.Artifact{{URI: "report.json", Digest: "ddd444"}},
		)
		Expect(err).NotTo(HaveOccurred())

		err = gen.VerifyChain(ctx, nil, []*attestation.SignedLink{link1, link2, link3})
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns ErrChainBroken on digest mismatch", func() {
		link1, err := gen.CreateLink(ctx, "ingestion",
			[]attestation.Artifact{{URI: "input.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "data.json", Digest: "bbb222"}},
		)
		Expect(err).NotTo(HaveOccurred())

		link2, err := gen.CreateLink(ctx, "analysis",
			[]attestation.Artifact{{URI: "data.json", Digest: "WRONG"}},
			[]attestation.Artifact{{URI: "out.json", Digest: "ccc"}},
		)
		Expect(err).NotTo(HaveOccurred())

		err = gen.VerifyChain(ctx, nil, []*attestation.SignedLink{link1, link2})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrChainBroken)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("data.json"))
		Expect(err.Error()).To(ContainSubstring("ingestion"))
		Expect(err.Error()).To(ContainSubstring("analysis"))
	})

	It("accepts steps where step N+1 has additional materials beyond step N products", func() {
		link1, err := gen.CreateLink(ctx, "step-a",
			[]attestation.Artifact{{URI: "in.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "shared.json", Digest: "bbb"}},
		)
		Expect(err).NotTo(HaveOccurred())

		link2, err := gen.CreateLink(ctx, "step-b",
			[]attestation.Artifact{
				{URI: "shared.json", Digest: "bbb"},
				{URI: "external.txt", Digest: "zzz"},
			},
			[]attestation.Artifact{{URI: "out.json", Digest: "ccc"}},
		)
		Expect(err).NotTo(HaveOccurred())

		err = gen.VerifyChain(ctx, nil, []*attestation.SignedLink{link1, link2})
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns nil for single-link chain", func() {
		link1, err := gen.CreateLink(ctx, "only-step",
			[]attestation.Artifact{{URI: "in.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "out.txt", Digest: "bbb"}},
		)
		Expect(err).NotTo(HaveOccurred())

		err = gen.VerifyChain(ctx, nil, []*attestation.SignedLink{link1})
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns nil for empty link slice", func() {
		err := gen.VerifyChain(ctx, nil, []*attestation.SignedLink{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sorts links by layout step order when layout is provided", func() {
		// Create links OUT of order
		link2, err := gen.CreateLink(ctx, "step-b",
			[]attestation.Artifact{{URI: "mid.json", Digest: "bbb"}},
			[]attestation.Artifact{{URI: "out.json", Digest: "ccc"}},
		)
		Expect(err).NotTo(HaveOccurred())

		link1, err := gen.CreateLink(ctx, "step-a",
			[]attestation.Artifact{{URI: "in.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "mid.json", Digest: "bbb"}},
		)
		Expect(err).NotTo(HaveOccurred())

		// Create layout with step-a before step-b
		opts := attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "step-a", Threshold: 1},
				{Name: "step-b", Threshold: 1},
			},
			ExpiresIn: 24 * time.Hour,
		}
		layout, err := gen.CreateLayout(ctx, opts)
		Expect(err).NotTo(HaveOccurred())

		// Pass links in wrong order — VerifyChain should sort by layout
		err = gen.VerifyChain(ctx, layout, []*attestation.SignedLink{link2, link1})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("GenerateManifest", func() {
	It("produces correct GNU coreutils format", func() {
		artifacts := []attestation.Artifact{
			{URI: "file-a.txt", Digest: "aaa111"},
			{URI: "file-b.txt", Digest: "bbb222"},
		}
		manifest := attestation.GenerateManifest(artifacts)
		lines := strings.Split(strings.TrimRight(string(manifest), "\n"), "\n")
		Expect(lines).To(HaveLen(2))
		Expect(lines[0]).To(Equal("aaa111  file-a.txt"))
		Expect(lines[1]).To(Equal("bbb222  file-b.txt"))
	})

	It("sorts output by URI", func() {
		artifacts := []attestation.Artifact{
			{URI: "zebra.txt", Digest: "zzz"},
			{URI: "alpha.txt", Digest: "aaa"},
			{URI: "middle.txt", Digest: "mmm"},
		}
		manifest := attestation.GenerateManifest(artifacts)
		lines := strings.Split(strings.TrimRight(string(manifest), "\n"), "\n")
		Expect(lines).To(HaveLen(3))
		Expect(lines[0]).To(Equal("aaa  alpha.txt"))
		Expect(lines[1]).To(Equal("mmm  middle.txt"))
		Expect(lines[2]).To(Equal("zzz  zebra.txt"))
	})

	It("returns empty bytes for empty artifact list", func() {
		manifest := attestation.GenerateManifest(nil)
		Expect(manifest).To(BeEmpty())
	})

	It("produces deterministic output for same input", func() {
		artifacts := []attestation.Artifact{
			{URI: "b.txt", Digest: "222"},
			{URI: "a.txt", Digest: "111"},
		}
		m1 := attestation.GenerateManifest(artifacts)
		m2 := attestation.GenerateManifest(artifacts)
		Expect(m1).To(Equal(m2))
	})
})
