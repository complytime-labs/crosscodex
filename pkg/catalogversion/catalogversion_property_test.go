package catalogversion_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/catalogversion"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("renders slug, label and DateAndHash in one fixed shape", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			slug := rapid.StringMatching(`[a-z0-9][a-z0-9-]{0,62}`).Draw(t, "slug")
			label := rapid.String().Draw(t, "label")
			hash := rapid.StringMatching(`[0-9a-f]{0,64}`).Draw(t, "hash")
			at := time.Unix(rapid.Int64Range(0, 4102444800).Draw(t, "unix"), 0)

			got := catalogversion.Format(slug, label, at, hash)
			want := slug + " @ " + label + " (" + catalogversion.DateAndHash(at, hash) + ")"
			if got != want {
				t.Fatalf("Format = %q, want %q", got, want)
			}
			suffix := strings.TrimPrefix(catalogversion.DateAndHash(at, hash), at.UTC().Format("2006-01-02")+" · ")
			if len(suffix) > 6 || !strings.HasPrefix(hash, suffix) {
				t.Fatalf("hash part %q is not a prefix of at most 6 characters of %q", suffix, hash)
			}
		})
	})
})
