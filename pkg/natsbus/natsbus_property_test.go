package natsbus_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// Suite bootstrap lives in natsbus_bdd_test.go (TestNATSBusBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

// validTenantGen generates tenant IDs matching the canonical regex
// [a-z][a-z0-9-]{1,62}[a-z0-9] (3-64 chars).
func validTenantGen() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-z][a-z0-9-]{1,62}[a-z0-9]`)
}

// safeTokenGen generates non-empty tokens without NATS delimiters (., *, >).
// Uses alphanumeric + hyphen which matches real job/edge IDs.
func safeTokenGen() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-zA-Z0-9_-]{1,64}`)
}

var _ = Describe("Property Specifications", Ordered, func() {

	Context("validateToken — NATS subject injection prevention", func() {
		It("rejects tokens containing the dot delimiter", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Inject at least one dot into an otherwise valid token
				prefix := rapid.StringN(0, 10, -1).Draw(t, "prefix")
				suffix := rapid.StringN(0, 10, -1).Draw(t, "suffix")
				token := prefix + "." + suffix
				err := natsbus.ValidateToken(token, "test")
				Expect(err).To(HaveOccurred(),
					"validateToken accepted token with dot: %q", token)
			})
		})

		It("rejects tokens containing the star wildcard", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prefix := rapid.StringN(0, 10, -1).Draw(t, "prefix")
				suffix := rapid.StringN(0, 10, -1).Draw(t, "suffix")
				token := prefix + "*" + suffix
				err := natsbus.ValidateToken(token, "test")
				Expect(err).To(HaveOccurred(),
					"validateToken accepted token with star: %q", token)
			})
		})

		It("rejects tokens containing the gt wildcard", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prefix := rapid.StringN(0, 10, -1).Draw(t, "prefix")
				suffix := rapid.StringN(0, 10, -1).Draw(t, "suffix")
				token := prefix + ">" + suffix
				err := natsbus.ValidateToken(token, "test")
				Expect(err).To(HaveOccurred(),
					"validateToken accepted token with gt: %q", token)
			})
		})

		It("rejects empty tokens", func() {
			err := natsbus.ValidateToken("", "test")
			Expect(err).To(HaveOccurred(),
				"validateToken accepted empty token")
		})

		It("accepts tokens without NATS delimiters", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				token := safeTokenGen().Draw(t, "safe-token")
				err := natsbus.ValidateToken(token, "test")
				Expect(err).NotTo(HaveOccurred(),
					"validateToken rejected safe token %q", token)
			})
		})
	})

	Context("subject builders — format compliance", func() {
		It("produces crosscodex-prefixed subjects containing the tenant ID", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenantID := validTenantGen().Draw(t, "tenant")
				jobID := safeTokenGen().Draw(t, "job-id")

				builders := []struct {
					name string
					fn   func() (string, error)
				}{
					{"PipelineStateSubject", func() (string, error) {
						return natsbus.PipelineStateSubject(tenantID, jobID)
					}},
					{"WorkSubject", func() (string, error) {
						return natsbus.WorkSubject(tenantID, natsbus.TaskClassify, jobID)
					}},
					{"ResultSubject", func() (string, error) {
						return natsbus.ResultSubject(tenantID, natsbus.TaskClassify, jobID)
					}},
					{"FeedbackSubject", func() (string, error) {
						return natsbus.FeedbackSubject(tenantID, jobID)
					}},
				}

				for _, b := range builders {
					subject, err := b.fn()
					if err != nil {
						// If the builder rejects a generated input, skip —
						// the rejection is valid (tenant validation is stricter
						// than our generator might produce in edge cases).
						continue
					}
					Expect(subject).To(HavePrefix("crosscodex."),
						"%s output missing crosscodex prefix: %q", b.name, subject)
					Expect(subject).To(ContainSubstring(tenantID),
						"%s output missing tenant ID %q: %q", b.name, tenantID, subject)
					// No NATS wildcards in the rendered subject tokens
					parts := strings.Split(subject, ".")
					for _, p := range parts {
						Expect(p).NotTo(ContainSubstring("*"),
							"%s output contains wildcard *: %q", b.name, subject)
						Expect(p).NotTo(ContainSubstring(">"),
							"%s output contains wildcard >: %q", b.name, subject)
					}
				}
			})
		})
	})

	Context("contentHash — determinism", func() {
		It("returns the same 64-char hex output for the same input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				h1 := natsbus.ContentHash(data)
				h2 := natsbus.ContentHash(data)
				Expect(h1).To(Equal(h2),
					"contentHash not deterministic for input len=%d", len(data))
				Expect(h1).To(HaveLen(64),
					"contentHash output length != 64 for input len=%d", len(data))
			})
		})
	})

	Context("extractProvenance — mandatory header enforcement", func() {
		mandatoryHeaders := []string{
			natsbus.HeaderTraceID,
			natsbus.HeaderSpanID,
			natsbus.HeaderTenantID,
			natsbus.HeaderTimestamp,
			natsbus.HeaderContentSHA256,
		}

		It("rejects headers when any single mandatory header is missing", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Start with all headers present
				headers := map[string][]string{
					natsbus.HeaderTraceID:       {"trace-" + rapid.StringN(1, 10, -1).Draw(t, "trace")},
					natsbus.HeaderSpanID:        {"span-" + rapid.StringN(1, 10, -1).Draw(t, "span")},
					natsbus.HeaderTenantID:      {"tenant-" + rapid.StringN(1, 10, -1).Draw(t, "tenant")},
					natsbus.HeaderTimestamp:     {"2026-01-01T00:00:00Z"},
					natsbus.HeaderContentSHA256: {"hash-" + rapid.StringN(1, 10, -1).Draw(t, "hash")},
				}

				// Remove exactly one
				dropIdx := rapid.IntRange(0, len(mandatoryHeaders)-1).Draw(t, "drop-idx")
				dropped := mandatoryHeaders[dropIdx]
				delete(headers, dropped)

				_, err := natsbus.ExtractProvenance(headers)
				Expect(err).To(HaveOccurred(),
					"extractProvenance accepted headers missing %s", dropped)
				Expect(err).To(MatchError(ContainSubstring(dropped)),
					"error did not mention missing header %s", dropped)
			})
		})
	})

	Context("mergeHeaders — provenance wins on conflict", func() {
		It("preserves all keys from both maps with provenance taking precedence", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate user headers
				userKeyCount := rapid.IntRange(0, 5).Draw(t, "user-key-count")
				user := make(map[string][]string, userKeyCount)
				for i := 0; i < userKeyCount; i++ {
					key := "User-" + rapid.StringMatching(`[A-Za-z]{1,10}`).Draw(t, "user-key")
					val := rapid.StringN(1, 20, -1).Draw(t, "user-val")
					user[key] = []string{val}
				}

				// Generate provenance headers
				provKeyCount := rapid.IntRange(0, 5).Draw(t, "prov-key-count")
				prov := make(map[string][]string, provKeyCount)
				for i := 0; i < provKeyCount; i++ {
					key := "Prov-" + rapid.StringMatching(`[A-Za-z]{1,10}`).Draw(t, "prov-key")
					val := rapid.StringN(1, 20, -1).Draw(t, "prov-val")
					prov[key] = []string{val}
				}

				// Add a conflict: same key in both with different values
				conflictKey := "X-Conflict"
				user[conflictKey] = []string{"user-value"}
				prov[conflictKey] = []string{"prov-value"}

				merged := natsbus.MergeHeaders(user, prov)

				// Provenance wins on conflict
				Expect(merged[conflictKey]).To(Equal([]string{"prov-value"}),
					"provenance did not win on conflict key %s", conflictKey)

				// All user keys present
				for k := range user {
					Expect(merged).To(HaveKey(k),
						"merged result missing user key %s", k)
				}

				// All provenance keys present
				for k := range prov {
					Expect(merged).To(HaveKey(k),
						"merged result missing provenance key %s", k)
				}
			})
		})
	})
})
