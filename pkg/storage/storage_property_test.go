package storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
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

	Context("NewTenantRouter — tenant routing", func() {
		It("opens once per distinct tenant and keeps each key in its tenant's provider", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				fakes := map[string]*storagetest.Fake{}
				opens := map[string]int{}
				route := storage.NewTenantRouter(func(id string) (storage.Provider, error) {
					opens[id]++
					fakes[id] = &storagetest.Fake{}
					return fakes[id], nil
				})
				defer route.Close()

				type put struct{ tenantID, key string }
				puts := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) put {
					return put{
						tenantID: fmt.Sprintf("tenant-%d", rapid.IntRange(1, 4).Draw(t, "tenant")),
						key:      rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "key"),
					}
				}), 1, 20).Draw(t, "puts")

				owners := map[string]map[string]bool{}
				for _, p := range puts {
					ctx, err := tenant.WithTenant(context.Background(), p.tenantID)
					if err != nil {
						t.Fatalf("WithTenant(%q): %v", p.tenantID, err)
					}
					if err := route.Put(ctx, p.key, strings.NewReader("x")); err != nil {
						t.Fatalf("Put(%q, %q): %v", p.tenantID, p.key, err)
					}
					if owners[p.key] == nil {
						owners[p.key] = map[string]bool{}
					}
					owners[p.key][p.tenantID] = true
				}

				for id, n := range opens {
					if n != 1 {
						t.Fatalf("open called %d times for %q, want 1", n, id)
					}
				}
				for id, fake := range fakes {
					for key := range fake.Objects() {
						if !owners[key][id] {
							t.Fatalf("key %q is in %q but was never put there", key, id)
						}
					}
				}
				for key, tenants := range owners {
					for id := range tenants {
						if _, ok := fakes[id].Objects()[key]; !ok {
							t.Fatalf("key %q missing from %q", key, id)
						}
					}
				}
			})
		})
	})
})
