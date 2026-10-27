package storage_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Suite bootstrap lives in storage_bdd_test.go (TestStorageBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("ValidateKey — path traversal prevention", func() {
		// joined draws two lowercase words and places sep between them.
		joined := func(sep string) func(*rapid.T) string {
			return func(t *rapid.T) string {
				prefix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "prefix")
				suffix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "suffix")
				return prefix + sep + suffix
			}
		}

		DescribeTable("rejects every generated key with a forbidden form",
			func(genKey func(*rapid.T) string, what string) {
				rapid.Check(GinkgoT(), func(t *rapid.T) {
					key := genKey(t)
					if err := storage.ValidateKey(key); err == nil {
						t.Fatalf("ValidateKey accepted %s key %q", what, key)
					}
				})
			},
			Entry("dot-dot path segments", joined("/../"), "path-traversal"),
			Entry("a leading slash", func(t *rapid.T) string {
				return "/" + rapid.StringMatching(`[a-z0-9/._-]{1,50}`).Draw(t, "rest")
			}, "absolute"),
			Entry("null bytes", joined("\x00"), "null-byte"),
			Entry("backslashes", joined(`\`), "backslash"),
		)
	})

	Context("ContentHash — determinism", func() {
		It("produces the same hash for the same input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				h1 := storage.ContentHash(data)
				h2 := storage.ContentHash(data)
				Expect(h1).To(Equal(h2),
					"ContentHash not deterministic")
				// Verify matches direct SHA-256 computation
				sum := sha256.Sum256(data)
				expected := hex.EncodeToString(sum[:])
				Expect(h1).To(Equal(expected),
					"ContentHash mismatch: got %q, want %q", h1, expected)
			})
		})
	})

	Context("ContentKey — format compliance", func() {
		It("always produces attestation/<hex64>.json format", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				key := storage.ContentKey(data)
				Expect(key).To(HavePrefix("attestation/"),
					"ContentKey missing prefix: %q", key)
				Expect(key).To(HaveSuffix(".json"),
					"ContentKey missing suffix: %q", key)
				// Middle part should be 64 hex chars
				middle := strings.TrimPrefix(key, "attestation/")
				middle = strings.TrimSuffix(middle, ".json")
				Expect(middle).To(HaveLen(64),
					"ContentKey hash wrong length: %d in %q", len(middle), key)
				_, err := hex.DecodeString(middle)
				Expect(err).NotTo(HaveOccurred(),
					"ContentKey hash not hex: %q", middle)
			})
		})
	})

	Context("JobAttestationKey — format compliance", func() {
		It("produces jobs/{jobID}/attestation/{filename} format", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				jobID := rapid.StringMatching(`[a-z0-9-]{3,36}`).Draw(t, "jobID")
				filename := rapid.StringMatching(`[a-z][a-z0-9._-]{0,49}`).Draw(t, "filename")
				key := storage.JobAttestationKey(jobID, filename)
				expected := fmt.Sprintf("jobs/%s/attestation/%s", jobID, filename)
				Expect(key).To(Equal(expected),
					"JobAttestationKey mismatch")
			})
		})
	})
})
