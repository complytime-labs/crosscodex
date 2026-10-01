//go:build integration

package main

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

const e2eCatalogFixture = "testdata/oscal/e2e-minimal-catalog.json"

// This suite contributes to the shared Ginkgo run tree driven by
// TestCrosscodexdIntegrationBDD in bootstrap_integration_test.go. See the
// comment in e2e_integration_test.go: RunSpecs may only be invoked once per
// test binary, so this file deliberately has no testing.T entry point.
//
// "locks the fixture's shape" test: the graph/relational assertions in the
// Tier 0 test depend on exactly these items existing.
var _ = Describe("E2E catalog fixture", func() {
	It("parses into the exact items the Tier 0 test depends on", func() {
		f, err := os.Open(e2eCatalogFixture)
		Expect(err).NotTo(HaveOccurred())
		defer f.Close()

		items, err := oscal.NewParser("").Parse(context.Background(), f)
		Expect(err).NotTo(HaveOccurred())

		byID := map[string]oscal.ControlItem{}
		for _, it := range items {
			byID[it.ID] = it
		}
		Expect(items).To(HaveLen(3), "item count: got %d, want 3 (%v)", len(items), byID)

		parent, ok := byID["ac-2"]
		Expect(ok && parent.Class == oscal.ClassSection && parent.ParentID == "").To(BeTrue(),
			"ac-2: got %+v, want ClassSection with no parent", parent)

		for _, child := range []string{"ac-2.a", "ac-2.b"} {
			c, ok := byID[child]
			Expect(ok && c.ParentID == "ac-2" && c.Class == oscal.ClassRequirement).To(BeTrue(),
				"%s: got %+v, want ClassRequirement child of ac-2", child, c)
		}

		Expect(byID["ac-2.a"].Text == byID["ac-2.b"].Text && byID["ac-2.a"].Text != "").To(BeTrue(),
			"children must share identical non-empty Text to form a requires candidate pair")
	})
})
