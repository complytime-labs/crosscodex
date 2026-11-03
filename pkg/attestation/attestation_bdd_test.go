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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// tamperPayload returns the signed envelope raw with its payload's digest
// "aaa" changed to "bbb", leaving its signatures, key IDs included, as they
// were.
func tamperPayload(raw []byte) []byte {
	var env map[string]any
	ExpectWithOffset(1, json.Unmarshal(raw, &env)).To(Succeed())
	payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, string(payload)).To(ContainSubstring(`"aaa"`))
	env["payload"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(payload), `"aaa"`, `"bbb"`, 1)))
	out, err := json.Marshal(env)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return out
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

	Context("telling a key it does not trust from a bad signature", func() {
		var trusted, other attestation.Generator

		BeforeAll(func() {
			trustedKeys, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			otherKeys, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			trusted, err = attestation.NewGenerator(trustedKeys)
			Expect(err).NotTo(HaveOccurred())
			other, err = attestation.NewGenerator(otherKeys)
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns ErrUntrustedKey, still wrapping ErrVerificationFailed, for a link signed by another key", func() {
			ctx := context.Background()
			link, err := other.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = trusted.Verify(ctx, link.Raw)
			Expect(err).To(MatchError(attestation.ErrUntrustedKey))
			Expect(err).To(MatchError(attestation.ErrVerificationFailed))
		})

		It("names the key IDs the envelope's signatures carry, quoted, so an operator can compare them with retired keys", func() {
			ctx := context.Background()
			link, err := other.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())
			var env struct {
				Signatures []struct {
					KeyID string `json:"keyid"`
				} `json:"signatures"`
			}
			Expect(json.Unmarshal(link.Raw, &env)).To(Succeed())
			Expect(env.Signatures).To(HaveLen(1))
			otherID := env.Signatures[0].KeyID

			_, err = trusted.Verify(ctx, link.Raw)
			var untrusted *attestation.UntrustedKeyError
			Expect(errors.As(err, &untrusted)).To(BeTrue())
			Expect(untrusted.KeyIDs).To(Equal([]string{otherID}))
			Expect(err.Error()).To(ContainSubstring(strconv.Quote(otherID)))
		})

		It("keeps at most 4 key IDs, each cut to 128 bytes at a rune boundary, and counts the rest", func() {
			ctx := context.Background()
			link, err := other.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())
			var env map[string]any
			Expect(json.Unmarshal(link.Raw, &env)).To(Succeed())
			sig := env["signatures"].([]any)[0].(map[string]any)["sig"]
			// "a" then two-byte runes: byte 128 falls inside a rune.
			long := "a" + strings.Repeat("é", 200)
			var sigs []any
			for i := range 10 {
				sigs = append(sigs, map[string]any{"keyid": fmt.Sprintf("%d%s", i, long), "sig": sig})
			}
			env["signatures"] = sigs
			raw, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())

			_, err = trusted.Verify(ctx, raw)
			untrusted, ok := errors.AsType[*attestation.UntrustedKeyError](err)
			Expect(ok).To(BeTrue())
			Expect(untrusted.KeyIDs).To(HaveLen(4))
			for i, id := range untrusted.KeyIDs {
				Expect(len(id)).To(BeNumerically("<=", 128))
				Expect(utf8.ValidString(id)).To(BeTrue(), "key ID %d was cut inside a rune", i)
				Expect(id).To(HavePrefix(fmt.Sprintf("%da", i)))
			}
			Expect(untrusted.Omitted).To(Equal(6))
			Expect(err.Error()).To(ContainSubstring("and 6 more"))
			Expect(len(err.Error())).To(BeNumerically("<", 1024))
		})

		It("does not return ErrUntrustedKey for a tampered link signed by the trusted key", func() {
			ctx := context.Background()
			link, err := trusted.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = trusted.Verify(ctx, tamperPayload(link.Raw))
			Expect(err).To(MatchError(attestation.ErrVerificationFailed))
			Expect(err).NotTo(MatchError(attestation.ErrUntrustedKey))
		})

		It("does not return ErrUntrustedKey for a bad signature that names the trusted key ID", func() {
			ctx := context.Background()
			// mockKeyProvider names every key "mock-key-id", so gen2's
			// signature claims the trusted key ID but does not verify.
			gen2, err := attestation.NewGenerator(newMockKeyProvider())
			Expect(err).NotTo(HaveOccurred())
			link, err := gen2.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = gen.Verify(ctx, link.Raw)
			Expect(err).To(MatchError(attestation.ErrVerificationFailed))
			Expect(err).NotTo(MatchError(attestation.ErrUntrustedKey))
		})

		It("returns ErrUntrustedKey for an envelope with no signature", func() {
			ctx := context.Background()
			link, err := trusted.CreateLink(ctx, "test", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
			Expect(err).NotTo(HaveOccurred())
			var env map[string]any
			Expect(json.Unmarshal(link.Raw, &env)).To(Succeed())
			env["signatures"] = []any{}
			unsigned, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())

			_, err = trusted.Verify(ctx, unsigned)
			Expect(err).To(MatchError(attestation.ErrUntrustedKey))
		})
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

		run := attestation.Artifact{URI: "job:t1/j1", Digest: "sha256:cfg"}
		link1, err := localGen.CreateLink(context.Background(), "step-a",
			[]attestation.Artifact{run, {URI: "in.txt", Digest: "aaa"}},
			[]attestation.Artifact{{URI: "mid.json", Digest: "bbb"}},
		)
		Expect(err).NotTo(HaveOccurred())
		link2, err := localGen.CreateLink(context.Background(), "step-b",
			[]attestation.Artifact{run, {URI: "mid.json", Digest: "bbb"}},
			[]attestation.Artifact{{URI: "out.txt", Digest: "ccc"}},
		)
		Expect(err).NotTo(HaveOccurred())
		layout, err := localGen.CreateLayout(context.Background(), attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "step-a", ExpectedMaterials: []string{run.URI}, Threshold: 1},
				{Name: "step-b", ExpectedMaterials: []string{run.URI}, Threshold: 1},
			},
			ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())

		localTP.Reset()
		_, err = localGen.VerifyChain(context.Background(), layout, []*attestation.SignedLink{link1, link2}, run)
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

	DescribeTable("accepts an ECDSA P-256 key",
		func(fipsMode bool) {
			kp := newMockKeyProvider()
			gen, err := attestation.NewGenerator(kp, attestation.WithFIPSMode(fipsMode))
			Expect(err).NotTo(HaveOccurred())

			materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
			products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
			_, err = gen.CreateLink(ctx, "step", materials, products)
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("allows ECDSA P-256 key when FIPS mode enabled", true),
		Entry("skips FIPS check when FIPS mode disabled", false),
	)

	It("rejects non-ECDSA key with ErrNonFIPSAlgorithm", func() {
		err := attestation.ValidateFIPSKey(newEd25519Signer())
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, attestation.ErrNonFIPSAlgorithm)).To(BeTrue())
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

	// verifiedByProducts creates and verifies a link from a generator built
	// with the given includeByProducts flag and returns the verified ByProducts.
	verifiedByProducts := func(includeByProducts bool) map[string]any {
		kp := newMockKeyProvider()
		gen, err := attestation.NewGenerator(kp, attestation.WithIncludeByProducts(includeByProducts))
		Expect(err).NotTo(HaveOccurred())

		materials := []attestation.Artifact{{URI: "in.txt", Digest: "aaa"}}
		products := []attestation.Artifact{{URI: "out.txt", Digest: "bbb"}}
		link, err := gen.CreateLink(ctx, "step", materials, products)
		Expect(err).NotTo(HaveOccurred())

		verified, err := gen.Verify(ctx, link.Raw)
		Expect(err).NotTo(HaveOccurred())
		return verified.ByProducts
	}

	DescribeTable("ByProducts key presence by includeByProducts flag",
		func(includeByProducts bool, wantKeys, absentKeys []string) {
			byProducts := verifiedByProducts(includeByProducts)
			for _, k := range wantKeys {
				Expect(byProducts).To(HaveKey(k))
			}
			for _, k := range absentKeys {
				Expect(byProducts).NotTo(HaveKey(k))
			}
		},
		Entry("always includes trace_id regardless of includeByProducts flag", false, []string{"trace_id"}, nil),
		Entry("includes span_id when includeByProducts is true", true, []string{"span_id"}, nil),
		Entry("includes hostname when includeByProducts is true", true, []string{"hostname"}, nil),
		Entry("does not include span_id, timestamp or hostname when includeByProducts is false", false, nil, []string{"span_id", "timestamp", "hostname"}),
	)

	It("includes timestamp in RFC3339 format when includeByProducts is true", func() {
		ts, ok := verifiedByProducts(true)["timestamp"].(string)
		Expect(ok).To(BeTrue())
		_, parseErr := time.Parse(time.RFC3339, ts)
		Expect(parseErr).NotTo(HaveOccurred())
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

	It("reports only the key IDs whose signatures verified, not every signature entry", func() {
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		signer, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		layout, err := signer.CreateLayout(ctx, attestation.LayoutOptions{
			Steps: []attestation.Step{{Name: "build", Threshold: 1}}, ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
		var env map[string]any
		Expect(json.Unmarshal(layout.Raw, &env)).To(Succeed())
		env["signatures"] = append(env["signatures"].([]any), map[string]any{"keyid": "bogus-key", "sig": "AAAA"})
		withBogus, err := json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())

		verified, err := signer.VerifyLayout(ctx, withBogus)
		Expect(err).NotTo(HaveOccurred())
		keyID, err := kp.KeyID(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(verified.KeyIDs).To(Equal([]string{keyID}))
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
		gen    attestation.Generator
		ctx    context.Context
		layout *attestation.SignedLayout
	)
	// subject names the run every layout step and link is bound to.
	subject := attestation.Artifact{URI: "job:t1/j1", Digest: "sha256:cfg"}

	// The layout is a → b → c, where c also consumes a's product directly,
	// so a link pair that is not adjacent in layout order shares an artifact.
	BeforeEach(func() {
		ctx = context.Background()
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		layout, err = gen.CreateLayout(ctx, attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "a", ExpectedMaterials: []string{subject.URI}, ExpectedProducts: []string{"a.out"}, Threshold: 1},
				{Name: "b", ExpectedMaterials: []string{subject.URI, "a.out"}, ExpectedProducts: []string{"b.out"}, Threshold: 1},
				{Name: "c", ExpectedMaterials: []string{subject.URI, "a.out", "b.out"}, ExpectedProducts: []string{"c.out"}, Threshold: 1},
			},
			ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// linkFor signs a link for step that records run as its subject.
	linkFor := func(g attestation.Generator, run attestation.Artifact, step string, materials, products []attestation.Artifact) *attestation.SignedLink {
		l, err := g.CreateLink(ctx, step, append([]attestation.Artifact{run}, materials...), products)
		Expect(err).NotTo(HaveOccurred())
		return l
	}
	link := func(g attestation.Generator, step string, materials, products []attestation.Artifact) *attestation.SignedLink {
		return linkFor(g, subject, step, materials, products)
	}
	verify := func(l *attestation.SignedLayout, links []*attestation.SignedLink) error {
		_, err := gen.VerifyChain(ctx, l, links, subject)
		return err
	}
	art := func(uri, digest string) attestation.Artifact { return attestation.Artifact{URI: uri, Digest: digest} }
	arts := func(a ...attestation.Artifact) []attestation.Artifact { return a }
	linkA := func() *attestation.SignedLink { return link(gen, "a", nil, arts(art("a.out", "aaa"))) }
	linkB := func() *attestation.SignedLink {
		return link(gen, "b", arts(art("a.out", "aaa")), arts(art("b.out", "bbb")))
	}
	linkC := func() *attestation.SignedLink {
		return link(gen, "c", arts(art("a.out", "aaa"), art("b.out", "bbb")), arts(art("c.out", "ccc")))
	}

	It("accepts one link per layout step, in any order", func() {
		Expect(verify(layout, []*attestation.SignedLink{linkC(), linkA(), linkB()})).To(Succeed())
	})

	It("reads steps, materials and products from the signed envelopes, not the SignedLink fields", func() {
		forged := linkB()
		forged.Step = "c"
		forged.Products = arts(art("b.out", "not-what-was-signed"))
		Expect(verify(layout, []*attestation.SignedLink{linkA(), forged, linkC()})).To(Succeed())
	})

	It("rejects a missing layout", func() {
		err := verify(nil, []*attestation.SignedLink{linkA()})
		Expect(err).To(MatchError(attestation.ErrInvalidLayout))
	})

	It("rejects a layout signed with another key", func() {
		otherKP, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		other, err := attestation.NewGenerator(otherKP)
		Expect(err).NotTo(HaveOccurred())
		foreign, err := other.CreateLayout(ctx, attestation.LayoutOptions{
			Steps: []attestation.Step{{Name: "a", Threshold: 1}}, ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())

		err = verify(foreign, []*attestation.SignedLink{linkA()})
		Expect(err).To(MatchError(attestation.ErrVerificationFailed))
		Expect(err.Error()).To(ContainSubstring("layout"))
	})

	It("rejects a layout step without a link, naming the step", func() {
		err := verify(layout, []*attestation.SignedLink{linkA(), linkC()})
		Expect(err).To(MatchError(attestation.ErrMissingLink))
		Expect(err.Error()).To(ContainSubstring(`"b"`))
	})

	It("rejects an empty link set", func() {
		err := verify(layout, nil)
		Expect(err).To(MatchError(attestation.ErrMissingLink))
	})

	DescribeTable("rejects links the layout does not declare, naming the step",
		func(extra func() *attestation.SignedLink, step string) {
			err := verify(layout, []*attestation.SignedLink{linkA(), linkB(), linkC(), extra()})
			Expect(err).To(MatchError(attestation.ErrUnexpectedLink))
			Expect(err.Error()).To(ContainSubstring(`"` + step + `"`))
		},
		Entry("a second link for a step", func() *attestation.SignedLink { return linkB() }, "b"),
		Entry("a link for a step outside the layout", func() *attestation.SignedLink {
			return link(gen, "rogue", nil, arts(art("rogue.out", "rrr")))
		}, "rogue"),
	)

	It("rejects a link signed with another key, naming its step", func() {
		otherKP, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		other, err := attestation.NewGenerator(otherKP)
		Expect(err).NotTo(HaveOccurred())
		foreignB := link(other, "b", arts(art("a.out", "aaa")), arts(art("b.out", "bbb")))

		err = verify(layout, []*attestation.SignedLink{linkA(), foreignB, linkC()})
		Expect(err).To(MatchError(attestation.ErrVerificationFailed))
		Expect(err.Error()).To(ContainSubstring(`"b"`))
	})

	It("rejects a link whose signed bytes were modified, naming its step", func() {
		tampered := linkB()
		var env map[string]any
		Expect(json.Unmarshal(tampered.Raw, &env)).To(Succeed())
		payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(payload)).To(ContainSubstring("bbb"))
		env["payload"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(payload), "bbb", "bbc", 1)))
		tampered.Raw, err = json.Marshal(env)
		Expect(err).NotTo(HaveOccurred())

		err = verify(layout, []*attestation.SignedLink{linkA(), tampered, linkC()})
		Expect(err).To(MatchError(attestation.ErrVerificationFailed))
		Expect(err.Error()).To(ContainSubstring(`"b"`))
	})

	It("rejects a digest mismatch between steps that are not adjacent in layout order, naming both and the artifact", func() {
		badC := link(gen, "c", arts(art("a.out", "WRONG"), art("b.out", "bbb")), arts(art("c.out", "ccc")))

		err := verify(layout, []*attestation.SignedLink{linkA(), linkB(), badC})
		Expect(err).To(MatchError(attestation.ErrChainBroken))
		Expect(err.Error()).To(And(ContainSubstring(`"a"`), ContainSubstring(`"c"`), ContainSubstring(`"a.out"`)))
	})

	DescribeTable("rejects a link that lacks an artifact its layout step expects, naming the step and the artifact",
		func(bad func() *attestation.SignedLink, uri string) {
			err := verify(layout, []*attestation.SignedLink{linkA(), bad(), linkC()})
			Expect(err).To(MatchError(attestation.ErrChainBroken))
			Expect(err.Error()).To(And(ContainSubstring(`"b"`), ContainSubstring(`"`+uri+`"`)))
		},
		Entry("an expected material", func() *attestation.SignedLink {
			return link(gen, "b", nil, arts(art("b.out", "bbb")))
		}, "a.out"),
		Entry("an expected product", func() *attestation.SignedLink {
			return link(gen, "b", arts(art("a.out", "aaa")), nil)
		}, "b.out"),
	)

	It("returns the verified layout and the subject the links record", func() {
		chain, err := gen.VerifyChain(ctx, layout, []*attestation.SignedLink{linkA(), linkB(), linkC()}, subject)
		Expect(err).NotTo(HaveOccurred())
		Expect(chain.Subject).To(Equal(subject))
		Expect(chain.Layout.Steps).To(HaveLen(3))
		Expect(chain.Layout.Steps[0].Name).To(Equal("a"))
	})

	It("rejects an empty subject, since a chain bound to no run proves nothing about any", func() {
		_, err := gen.VerifyChain(ctx, layout, []*attestation.SignedLink{linkA(), linkB(), linkC()}, attestation.Artifact{})
		Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
	})

	It("rejects a link replayed from another run, naming the step and the run it records", func() {
		replayed := linkFor(gen, art("job:t1/other", "sha256:cfg"), "b", arts(art("a.out", "aaa")), arts(art("b.out", "bbb")))
		err := verify(layout, []*attestation.SignedLink{linkA(), replayed, linkC()})
		Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
		Expect(err.Error()).To(And(ContainSubstring(`"b"`), ContainSubstring(`"job:t1/j1"`)))
	})

	It("rejects a link that records the run with another digest, naming the step and both digests", func() {
		stale := linkFor(gen, art(subject.URI, "sha256:other-config"), "b", arts(art("a.out", "aaa")), arts(art("b.out", "bbb")))
		err := verify(layout, []*attestation.SignedLink{linkA(), stale, linkC()})
		Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
		Expect(err.Error()).To(And(ContainSubstring(`"b"`), ContainSubstring("sha256:other-config"), ContainSubstring("sha256:cfg")))
	})

	It("rejects a layout whose steps do not name the run, naming the step", func() {
		other, err := gen.CreateLayout(ctx, attestation.LayoutOptions{
			Steps: []attestation.Step{
				{Name: "a", ExpectedProducts: []string{"a.out"}, Threshold: 1},
				{Name: "b", ExpectedMaterials: []string{subject.URI, "a.out"}, ExpectedProducts: []string{"b.out"}, Threshold: 1},
				{Name: "c", ExpectedMaterials: []string{subject.URI, "a.out", "b.out"}, ExpectedProducts: []string{"c.out"}, Threshold: 1},
			},
			ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
		err = verify(other, []*attestation.SignedLink{linkA(), linkB(), linkC()})
		Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
		Expect(err.Error()).To(ContainSubstring(`"a"`))
	})

	Context("when the caller knows the run but not its digest", func() {
		uriOnly := attestation.Artifact{URI: subject.URI}

		It("accepts links that agree on the digest and returns it", func() {
			chain, err := gen.VerifyChain(ctx, layout, []*attestation.SignedLink{linkA(), linkB(), linkC()}, uriOnly)
			Expect(err).NotTo(HaveOccurred())
			Expect(chain.Subject).To(Equal(subject))
		})

		It("rejects links that disagree on it", func() {
			stale := linkFor(gen, art(subject.URI, "sha256:other-config"), "b", arts(art("a.out", "aaa")), arts(art("b.out", "bbb")))
			_, err := gen.VerifyChain(ctx, layout, []*attestation.SignedLink{linkA(), stale, linkC()}, uriOnly)
			Expect(err).To(MatchError(attestation.ErrSubjectMismatch))
		})
	})

	It("leaves the caller's links unchanged", func() {
		links := []*attestation.SignedLink{linkC(), linkA(), linkB()}
		before := []string{links[0].Step, links[1].Step, links[2].Step}
		Expect(verify(layout, links)).To(Succeed())
		Expect([]string{links[0].Step, links[1].Step, links[2].Step}).To(Equal(before))
	})
})

var _ = Describe("LoadFileKeys", func() {
	var dir string

	writeKey := func(name, pemType string, der []byte) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der}), 0o600)).To(Succeed())
		return path
	}
	writePair := func(curve elliptic.Curve, prefix string) (string, string) {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		privDER, err := x509.MarshalECPrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		return writeKey(prefix+"-key.pem", "EC PRIVATE KEY", privDER), writeKey(prefix+"-pub.pem", "PUBLIC KEY", pubDER)
	}

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("signs with the loaded key, and the link verifies against the public key file", func() {
		priv, pub := writePair(elliptic.P256(), "ok")
		kp, err := attestation.LoadFileKeys(priv, pub, false)
		Expect(err).NotTo(HaveOccurred())
		gen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		signed, err := gen.CreateLink(context.Background(), "s", nil, nil)
		Expect(err).NotTo(HaveOccurred())

		verifier, err := attestation.NewGenerator(&attestation.FileKeyProvider{PublicKeyPath: pub})
		Expect(err).NotTo(HaveOccurred())
		_, err = verifier.Verify(context.Background(), signed.Raw)
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads the key files once, so signing does not depend on them afterwards", func() {
		priv, pub := writePair(elliptic.P256(), "once")
		kp, err := attestation.LoadFileKeys(priv, pub, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.Remove(priv)).To(Succeed())
		Expect(os.Remove(pub)).To(Succeed())

		gen, err := attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		signed, err := gen.CreateLink(context.Background(), "s", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = gen.Verify(context.Background(), signed.Raw)
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects a missing private key file, naming the path", func() {
		_, pub := writePair(elliptic.P256(), "missing")
		absent := filepath.Join(dir, "absent.pem")
		_, err := attestation.LoadFileKeys(absent, pub, false)
		Expect(err).To(MatchError(attestation.ErrKeyNotFound))
		Expect(err.Error()).To(ContainSubstring(absent))
	})

	It("rejects a public key file that does not parse, naming the path", func() {
		priv, _ := writePair(elliptic.P256(), "garbled")
		garbled := filepath.Join(dir, "garbled.pem")
		Expect(os.WriteFile(garbled, []byte("not a key"), 0o600)).To(Succeed())
		_, err := attestation.LoadFileKeys(priv, garbled, false)
		Expect(err).To(MatchError(attestation.ErrKeyLoadFailed))
		Expect(err.Error()).To(ContainSubstring(garbled))
	})

	It("rejects a public key that is not the private key's, naming both paths", func() {
		priv, _ := writePair(elliptic.P256(), "one")
		_, otherPub := writePair(elliptic.P256(), "two")
		_, err := attestation.LoadFileKeys(priv, otherPub, false)
		Expect(err).To(MatchError(attestation.ErrKeyMismatch))
		Expect(err.Error()).To(And(ContainSubstring(priv), ContainSubstring(otherPub)))
	})

	It("rejects a key on a curve FIPS does not approve when FIPS mode is on", func() {
		priv, pub := writePair(elliptic.P224(), "p224")
		_, err := attestation.LoadFileKeys(priv, pub, true)
		Expect(err).To(MatchError(attestation.ErrNonFIPSAlgorithm))
		Expect(err.Error()).To(ContainSubstring(priv))
	})

	It("accepts the same key when FIPS mode is off", func() {
		priv, pub := writePair(elliptic.P224(), "p224-off")
		_, err := attestation.LoadFileKeys(priv, pub, false)
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Bundle encoding", func() {
	bundleRun := attestation.Artifact{URI: "job:t1/j1", Digest: "sha256:cfg"}
	var (
		gen    attestation.Generator
		ctx    context.Context
		layout *attestation.SignedLayout
		links  []*attestation.SignedLink
	)

	BeforeEach(func() {
		ctx = context.Background()
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		layout, err = gen.CreateLayout(ctx, attestation.LayoutOptions{
			Steps: []attestation.Step{{Name: "a", ExpectedMaterials: []string{bundleRun.URI}, ExpectedProducts: []string{"a.out"}, Threshold: 1}}, ExpiresIn: time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
		l, err := gen.CreateLink(ctx, "a", []attestation.Artifact{bundleRun}, []attestation.Artifact{{URI: "a.out", Digest: "aaa"}})
		Expect(err).NotTo(HaveOccurred())
		links = []*attestation.SignedLink{l}
	})

	It("round-trips a layout and its links, which then verify as a chain", func() {
		data, err := attestation.MarshalBundle(layout, links)
		Expect(err).NotTo(HaveOccurred())

		gotLayout, gotLinks, err := attestation.ParseBundle(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(gotLayout.Raw).To(MatchJSON(layout.Raw))
		Expect(gotLinks).To(HaveLen(1))
		Expect(gotLinks[0].Raw).To(MatchJSON(links[0].Raw))
		_, err = gen.VerifyChain(ctx, gotLayout, gotLinks, bundleRun)
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("rejects input that is not a bundle, saying what a bundle is",
		func(data string) {
			_, _, err := attestation.ParseBundle([]byte(data))
			Expect(err).To(MatchError(attestation.ErrInvalidBundle))
			Expect(err.Error()).To(ContainSubstring(`"layout"`))
		},
		Entry("not JSON", `not json`),
		Entry("a JSON array", `[]`),
		Entry("a single signed envelope", `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}`),
		Entry("no layout", `{"links":[{}]}`),
		Entry("no links", `{"layout":{}}`),
		Entry("an empty link list", `{"layout":{},"links":[]}`),
		Entry("a null link", `{"layout":{},"links":[null]}`),
	)

	It("refuses to encode a bundle without a layout or links", func() {
		_, err := attestation.MarshalBundle(nil, links)
		Expect(err).To(MatchError(attestation.ErrInvalidBundle))
		_, err = attestation.MarshalBundle(layout, nil)
		Expect(err).To(MatchError(attestation.ErrInvalidBundle))
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
