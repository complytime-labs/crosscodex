//go:build !integration

package catalog_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/catalog"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("DiffControls", func() {
		controls := rapid.SliceOf(rapid.Custom(func(t *rapid.T) catalog.ControlRecord {
			return catalog.ControlRecord{
				Identifier: rapid.StringMatching(`[a-d][0-9]`).Draw(t, "id"),
				Statement:  rapid.StringMatching(`[xyz]{0,2}`).Draw(t, "statement"),
			}
		}))

		It("reports no differences when comparing a version with itself", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				x := controls.Draw(t, "x")
				got := catalog.DiffControls(x, x)
				Expect(got.Added).To(BeEmpty())
				Expect(got.Removed).To(BeEmpty())
				Expect(got.Changed).To(BeEmpty())
			})
		})

		It("mirrors added and removed when the direction is reversed", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				prev := controls.Draw(t, "prev")
				next := controls.Draw(t, "next")
				fwd := catalog.DiffControls(prev, next)
				rev := catalog.DiffControls(next, prev)
				Expect(fwd.Added).To(Equal(rev.Removed))
				Expect(fwd.Removed).To(Equal(rev.Added))
				Expect(fwd.Changed).To(Equal(rev.Changed))
			})
		})
	})
})
