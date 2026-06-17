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
	Context("validateKey — path traversal prevention", func() {
		It("rejects keys containing dot-dot path segments", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prefix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "prefix")
				suffix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "suffix")
				key := prefix + "/../" + suffix
				err := storage.ExportValidateKey(key)
				Expect(err).To(HaveOccurred(),
					"validateKey accepted path-traversal key %q", key)
			})
		})

		It("rejects keys starting with /", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				rest := rapid.StringMatching(`[a-z0-9/._-]{1,50}`).Draw(t, "rest")
				key := "/" + rest
				err := storage.ExportValidateKey(key)
				Expect(err).To(HaveOccurred(),
					"validateKey accepted absolute key %q", key)
			})
		})

		It("rejects keys containing null bytes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prefix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "prefix")
				suffix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "suffix")
				key := prefix + "\x00" + suffix
				err := storage.ExportValidateKey(key)
				Expect(err).To(HaveOccurred(),
					"validateKey accepted null-byte key %q", key)
			})
		})

		It("rejects keys containing backslashes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prefix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "prefix")
				suffix := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "suffix")
				key := prefix + `\` + suffix
				err := storage.ExportValidateKey(key)
				Expect(err).To(HaveOccurred(),
					"validateKey accepted backslash key %q", key)
			})
		})
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
