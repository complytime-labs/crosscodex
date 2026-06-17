package tlsconfig_test

import (
	"crypto/tls"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

var _ = Describe("Property Specifications", func() {

	Context("mergeConfig — override semantics", func() {

		It("returns global config when target is empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				mode := rapid.SampledFrom([]string{"", "off", "server-only", "mutual"}).Draw(t, "mode")
				ca := rapid.StringN(0, 50, -1).Draw(t, "ca")
				cert := rapid.StringN(0, 50, -1).Draw(t, "cert")
				key := rapid.StringN(0, 50, -1).Draw(t, "key")

				cfg := config.TLSConfig{
					Mode: mode,
					CA:   ca,
					Cert: cert,
					Key:  key,
					Targets: map[string]config.TLSOverride{
						"some-target": {Mode: "mutual", CA: "/other/ca"},
					},
				}

				gotMode, gotCA, gotCert, gotKey := tlsconfig.MergeConfigFields(cfg, "")
				Expect(gotMode).To(Equal(mode))
				Expect(gotCA).To(Equal(ca))
				Expect(gotCert).To(Equal(cert))
				Expect(gotKey).To(Equal(key))
			})
		})

		It("returns global config when target does not exist in Targets map", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				mode := rapid.SampledFrom([]string{"", "off", "server-only", "mutual"}).Draw(t, "mode")
				ca := rapid.StringN(0, 50, -1).Draw(t, "ca")
				cert := rapid.StringN(0, 50, -1).Draw(t, "cert")
				key := rapid.StringN(0, 50, -1).Draw(t, "key")
				unmatched := rapid.StringMatching(`[a-z]{5,15}`).Draw(t, "unmatched-target")

				cfg := config.TLSConfig{
					Mode: mode,
					CA:   ca,
					Cert: cert,
					Key:  key,
					// No Targets map entry for the generated name.
				}

				gotMode, gotCA, gotCert, gotKey := tlsconfig.MergeConfigFields(cfg, unmatched)
				Expect(gotMode).To(Equal(mode))
				Expect(gotCA).To(Equal(ca))
				Expect(gotCert).To(Equal(cert))
				Expect(gotKey).To(Equal(key))
			})
		})
	})

	Context("filterCiphers — subset and filtering", func() {

		It("result is always a subset of the base set", func() {
			allIDs := allCipherIDs()

			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Draw a non-empty subset of all cipher IDs as the base.
				base := drawCipherSubset(t, allIDs)

				result, err := tlsconfig.FilterCiphers(base, nil, nil)
				Expect(err).NotTo(HaveOccurred())

				baseSet := idSet(base)
				for _, id := range result {
					Expect(baseSet).To(HaveKey(id), "result cipher %d not in base set", id)
				}
			})
		})

		It("filtering is idempotent — filtering twice equals filtering once", func() {
			allIDs := allCipherIDs()
			allNames := cipherNameMap()

			rapid.Check(GinkgoT(), func(t *rapid.T) {
				base := drawCipherSubset(t, allIDs)

				// Draw allow/deny patterns from actual cipher name fragments.
				allow := drawCipherPatterns(t, allNames, "allow")
				deny := drawCipherPatterns(t, allNames, "deny")

				first, err1 := tlsconfig.FilterCiphers(base, allow, deny)
				if err1 != nil {
					// No ciphers survived — idempotency holds vacuously.
					return
				}

				second, err2 := tlsconfig.FilterCiphers(first, allow, deny)
				Expect(err2).NotTo(HaveOccurred())
				Expect(second).To(Equal(first))
			})
		})
	})

	Context("fipsCipherSuites — GCM-only enforcement", func() {

		It("every returned cipher name contains GCM", func() {
			nameByID := cipherNameMap()
			suites := tlsconfig.FipsCipherSuites()
			Expect(suites).NotTo(BeEmpty(), "FIPS suite list must not be empty")

			for _, id := range suites {
				name, ok := nameByID[id]
				Expect(ok).To(BeTrue(), "cipher ID %d not in tls.CipherSuites()", id)
				Expect(strings.Contains(name, "GCM")).To(BeTrue(),
					"FIPS cipher %s (0x%04x) does not contain GCM", name, id)
			}
		})
	})
})

// --- helpers ---

// allCipherIDs returns IDs of all non-insecure cipher suites.
func allCipherIDs() []uint16 {
	suites := tls.CipherSuites()
	ids := make([]uint16, len(suites))
	for i, cs := range suites {
		ids[i] = cs.ID
	}
	return ids
}

// cipherNameMap builds a map from cipher ID to name.
func cipherNameMap() map[uint16]string {
	m := make(map[uint16]string)
	for _, cs := range tls.CipherSuites() {
		m[cs.ID] = cs.Name
	}
	return m
}

// idSet converts a slice of IDs into a map for O(1) lookup.
func idSet(ids []uint16) map[uint16]struct{} {
	s := make(map[uint16]struct{}, len(ids))
	for _, id := range ids {
		s[id] = struct{}{}
	}
	return s
}

// drawCipherSubset draws a non-empty random subset of cipher IDs.
func drawCipherSubset(t *rapid.T, allIDs []uint16) []uint16 {
	perm := rapid.Permutation(allIDs).Draw(t, "perm")
	n := rapid.IntRange(1, len(perm)).Draw(t, "subset-size")
	return perm[:n]
}

// drawCipherPatterns draws 0-2 cipher name substring patterns from actual names.
func drawCipherPatterns(t *rapid.T, nameByID map[uint16]string, label string) []string {
	// Collect unique name fragments to use as patterns.
	fragments := []string{"GCM", "SHA256", "SHA384", "AES", "CHACHA20", "ECDHE", "RSA"}
	n := rapid.IntRange(0, 2).Draw(t, label+"-count")
	if n == 0 {
		return nil
	}
	patterns := make([]string, n)
	for i := 0; i < n; i++ {
		patterns[i] = rapid.SampledFrom(fragments).Draw(t, label+"-pattern")
	}
	return patterns
}
