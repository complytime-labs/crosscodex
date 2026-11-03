package analyzer_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

// Suite bootstrap lives in analyzer_bdd_test.go (TestAnalyzerBDD).
var _ = Describe("Property Specifications", Ordered, func() {
	Describe("ControlCatalogFromContext properties", func() {
		It("returns exactly the non-empty catalog recorded for each control", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				catalogs := rapid.MapOf(rapid.StringMatching(`[a-z]{1,8}/[A-Z]{1,3}-[0-9]{1,2}`), rapid.StringMatching(`[a-z0-9-]{0,8}`)).Draw(t, "catalogs")
				absent := rapid.StringMatching(`[a-z]{9,12}`).Draw(t, "absent")
				ctx := analyzer.WithControlCatalogs(context.Background(), catalogs)

				for controlID, want := range catalogs {
					got, ok := analyzer.ControlCatalogFromContext(ctx, controlID)
					if ok != (want != "") || got != want {
						t.Fatalf("ControlCatalogFromContext(%q) = (%q, %v), recorded %q", controlID, got, ok, want)
					}
				}
				if got, ok := analyzer.ControlCatalogFromContext(ctx, absent); ok {
					t.Fatalf("ControlCatalogFromContext(%q) = (%q, true) for an unrecorded control", absent, got)
				}
				if !analyzer.HasControlCatalogs(ctx) {
					t.Fatalf("HasControlCatalogs = false after WithControlCatalogs(%v)", catalogs)
				}
			})
		})
	})
})
