package attestation_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

// rsaSigner implements crypto.Signer with an RSA key for FIPS rejection testing.
type rsaSigner struct {
	key *rsa.PrivateKey
}

func (r *rsaSigner) Public() crypto.PublicKey {
	return r.key.Public()
}

func (r *rsaSigner) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	return nil, nil
}

// gnuCoreutilsLine matches the GNU coreutils sha256sum output format.
var gnuCoreutilsLine = regexp.MustCompile(`^[0-9a-f]{64} {2}.+$`)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("validateFIPSKey — algorithm enforcement", func() {
		It("accepts ECDSA P-256, P-384, and P-521 keys", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				curves := []elliptic.Curve{elliptic.P256(), elliptic.P384(), elliptic.P521()}
				idx := rapid.IntRange(0, len(curves)-1).Draw(t, "curve-index")
				key, err := ecdsa.GenerateKey(curves[idx], rand.Reader)
				Expect(err).NotTo(HaveOccurred())
				err = attestation.ValidateFIPSKey(key)
				Expect(err).NotTo(HaveOccurred(),
					"ValidateFIPSKey rejected FIPS-approved curve %s", curves[idx].Params().Name)
			})
		})

		It("rejects RSA keys", func() {
			// RSA keygen costs up to seconds per key, so generate one key per
			// size up front rather than one per rapid iteration.
			sizes := []int{2048, 3072, 4096}
			keys := map[int]*rsa.PrivateKey{}
			for _, bits := range sizes {
				key, err := rsa.GenerateKey(rand.Reader, bits)
				Expect(err).NotTo(HaveOccurred())
				keys[bits] = key
			}
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				bits := rapid.SampledFrom(sizes).Draw(t, "rsa-bits")
				err := attestation.ValidateFIPSKey(&rsaSigner{key: keys[bits]})
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(attestation.ErrNonFIPSAlgorithm))
			})
		})
	})

	Context("GenerateManifest — determinism", func() {
		It("produces the same output regardless of input order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 20).Draw(t, "artifact-count")
				artifacts := make([]attestation.Artifact, n)
				seen := make(map[string]bool)
				for i := range artifacts {
					var uri string
					for {
						uri = rapid.StringMatching(`[a-z][a-z0-9/._-]{0,30}`).Draw(t, "uri")
						if !seen[uri] {
							seen[uri] = true
							break
						}
					}
					artifacts[i] = attestation.Artifact{
						URI:    uri,
						Digest: rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "digest"),
					}
				}
				m1 := attestation.GenerateManifest(artifacts)

				// Reverse the order
				reversed := make([]attestation.Artifact, len(artifacts))
				for i, a := range artifacts {
					reversed[len(artifacts)-1-i] = a
				}
				m2 := attestation.GenerateManifest(reversed)

				Expect(string(m1)).To(Equal(string(m2)),
					"GenerateManifest produced different output for different orderings")
			})
		})
	})

	Context("GenerateManifest — non-mutation", func() {
		It("does not mutate the input slice", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 20).Draw(t, "artifact-count")
				artifacts := make([]attestation.Artifact, n)
				for i := range artifacts {
					artifacts[i] = attestation.Artifact{
						URI:    rapid.StringMatching(`[a-z][a-z0-9/._-]{0,30}`).Draw(t, "uri"),
						Digest: rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "digest"),
					}
				}

				// Snapshot original order
				original := make([]attestation.Artifact, len(artifacts))
				copy(original, artifacts)

				_ = attestation.GenerateManifest(artifacts)

				for i := range artifacts {
					Expect(artifacts[i]).To(Equal(original[i]),
						"GenerateManifest mutated input at index %d", i)
				}
			})
		})
	})

	Context("GenerateManifest — GNU coreutils format", func() {
		It("produces lines matching the sha256sum format", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 10).Draw(t, "artifact-count")
				artifacts := make([]attestation.Artifact, n)
				for i := range artifacts {
					artifacts[i] = attestation.Artifact{
						URI:    rapid.StringMatching(`[a-z][a-z0-9._-]{0,20}`).Draw(t, "uri"),
						Digest: rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "digest"),
					}
				}

				manifest := attestation.GenerateManifest(artifacts)
				lines := strings.Split(strings.TrimRight(string(manifest), "\n"), "\n")
				Expect(lines).To(HaveLen(n))
				for _, line := range lines {
					Expect(gnuCoreutilsLine.MatchString(line)).To(BeTrue(),
						"line does not match GNU coreutils format: %q", line)
				}
			})
		})
	})

	Context("fixCanonicalJSONNewlines — idempotency", func() {
		It("double application equals single application", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate JSON-like strings that may contain raw newlines
				input := rapid.SliceOfN(rapid.Byte(), 0, 200).Draw(t, "input")
				once := attestation.FixCanonicalJSONNewlines(input)
				twice := attestation.FixCanonicalJSONNewlines(once)
				Expect(twice).To(Equal(once),
					"fixCanonicalJSONNewlines is not idempotent")
			})
		})
	})

	Context("artifactsToHashObj/hashObjToArtifacts — roundtrip", func() {
		It("preserves URI and Digest pairs through conversion", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 20).Draw(t, "artifact-count")
				artifacts := make([]attestation.Artifact, n)
				seen := make(map[string]bool)
				for i := range artifacts {
					// Generate unique URIs to avoid map collision
					var uri string
					for {
						uri = rapid.StringMatching(`[a-z][a-z0-9/._-]{0,30}`).Draw(t, "uri")
						if !seen[uri] {
							seen[uri] = true
							break
						}
					}
					artifacts[i] = attestation.Artifact{
						URI:    uri,
						Digest: rapid.StringMatching(`[0-9a-f]{8,64}`).Draw(t, "digest"),
					}
				}

				hashObjs := attestation.ArtifactsToHashObj(artifacts)

				// Convert hashObjs to map[string]map[string]string for HashObjToArtifacts
				wrapped := make(map[string]map[string]string, len(hashObjs))
				for k, v := range hashObjs {
					wrapped[k] = map[string]string(v)
				}
				roundTripped := attestation.HashObjToArtifacts(wrapped)

				Expect(roundTripped).To(HaveLen(len(artifacts)))

				// Build lookup for comparison (order may differ)
				got := make(map[string]string, len(roundTripped))
				for _, a := range roundTripped {
					got[a.URI] = a.Digest
				}
				for _, a := range artifacts {
					Expect(got).To(HaveKeyWithValue(a.URI, a.Digest),
						"roundtrip lost artifact %q", a.URI)
				}
			})
		})
	})

	Context("stepsToInToto/inTotoStepsToSteps — roundtrip", func() {
		It("preserves Name and Threshold fields", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 10).Draw(t, "step-count")
				steps := make([]attestation.Step, n)
				for i := range steps {
					steps[i] = attestation.Step{
						Name:      rapid.StringMatching(`[a-z][a-z0-9-]{0,20}`).Draw(t, "name"),
						Threshold: rapid.IntRange(1, 10).Draw(t, "threshold"),
					}
				}

				itoSteps := attestation.StepsToInToto(steps)
				roundTripped := attestation.InTotoStepsToSteps(itoSteps)

				Expect(roundTripped).To(HaveLen(len(steps)))
				for i := range steps {
					Expect(roundTripped[i].Name).To(Equal(steps[i].Name),
						"Name mismatch at index %d", i)
					Expect(roundTripped[i].Threshold).To(Equal(steps[i].Threshold),
						"Threshold mismatch at index %d", i)
				}
			})
		})
	})
	Context("MarshalBundle/ParseBundle — roundtrip", func() {
		It("returns the layout and every link envelope, in order, byte for byte", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				envelope := func(label string) []byte {
					raw, err := json.Marshal(map[string]string{"payload": rapid.String().Draw(t, label)})
					if err != nil {
						t.Fatalf("marshal envelope: %v", err)
					}
					return raw
				}
				layout := &attestation.SignedLayout{Raw: envelope("layout")}
				n := rapid.IntRange(1, 8).Draw(t, "links")
				links := make([]*attestation.SignedLink, n)
				for i := range links {
					links[i] = &attestation.SignedLink{Raw: envelope(fmt.Sprintf("link-%d", i))}
				}

				data, err := attestation.MarshalBundle(layout, links)
				if err != nil {
					t.Fatalf("MarshalBundle: %v", err)
				}
				gotLayout, gotLinks, err := attestation.ParseBundle(data)
				if err != nil {
					t.Fatalf("ParseBundle: %v", err)
				}
				if !bytes.Equal(gotLayout.Raw, layout.Raw) {
					t.Fatalf("layout changed: %q != %q", gotLayout.Raw, layout.Raw)
				}
				if len(gotLinks) != n {
					t.Fatalf("got %d links, want %d", len(gotLinks), n)
				}
				for i := range links {
					if !bytes.Equal(gotLinks[i].Raw, links[i].Raw) {
						t.Fatalf("link %d changed: %q != %q", i, gotLinks[i].Raw, links[i].Raw)
					}
				}
			})
		})
	})

	Context("Verify — trusted and untrusted keys", func() {
		It("accepts a link only under its signer's key, and reports any other key as untrusted", func() {
			// Key generation is expensive: build a pool once, draw from it.
			gens := make([]attestation.Generator, 4)
			for i := range gens {
				kp, err := attestation.NewEphemeralKeyProvider()
				Expect(err).NotTo(HaveOccurred())
				gens[i], err = attestation.NewGenerator(kp)
				Expect(err).NotTo(HaveOccurred())
			}
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				signer := rapid.IntRange(0, len(gens)-1).Draw(t, "signer")
				verifier := rapid.IntRange(0, len(gens)-1).Draw(t, "verifier")
				digest := rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "digest")
				link, err := gens[signer].CreateLink(context.Background(), "step",
					[]attestation.Artifact{{URI: "a", Digest: digest}}, nil)
				if err != nil {
					t.Fatalf("CreateLink: %v", err)
				}
				_, err = gens[verifier].Verify(context.Background(), link.Raw)
				switch {
				case signer == verifier && err != nil:
					t.Fatalf("the signer's own key rejected its link: %v", err)
				case signer != verifier && !errors.Is(err, attestation.ErrUntrustedKey):
					t.Fatalf("another key's link: got %v, want ErrUntrustedKey", err)
				case signer != verifier && !errors.Is(err, attestation.ErrVerificationFailed):
					t.Fatalf("ErrUntrustedKey without ErrVerificationFailed: %v", err)
				}
			})
		})
	})
})
