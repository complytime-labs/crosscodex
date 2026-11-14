package crosswalk_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

const (
	syntheticCatalog   = "testdata/synthetic_catalog.json"
	statementCatalog   = "testdata/statement_catalog.json"
	sourceItemsCatalog = "testdata/source_items.json"
)

var _ = Describe("LoadCatalog", func() {
	It("loads a catalog and records the SHA-256 of its bytes", func() {
		c, err := crosswalk.LoadCatalog(syntheticCatalog)
		Expect(err).NotTo(HaveOccurred())
		raw, err := os.ReadFile(syntheticCatalog)
		Expect(err).NotTo(HaveOccurred())
		sum := sha256.Sum256(raw)
		Expect(c.Digest).To(Equal(hex.EncodeToString(sum[:])))
		Expect(c.Path).To(Equal(syntheticCatalog))
		Expect(c.Doc.Metadata.Title).To(Equal("Synthetic"))
	})

	It("names the path and the fetch task when the file is missing", func() {
		missing := filepath.Join(GinkgoT().TempDir(), "absent.json")
		_, err := crosswalk.LoadCatalog(missing)
		Expect(err).To(MatchError(And(ContainSubstring(missing), ContainSubstring("run task fetch:oscal-docs"))))
	})

	It("rejects a document without a catalog root, naming the path", func() {
		_, err := crosswalk.LoadCatalog("testdata/not_a_catalog.json")
		Expect(err).To(MatchError(And(ContainSubstring("testdata/not_a_catalog.json"), ContainSubstring("catalog"))))
	})

	It("rejects malformed JSON, naming the path", func() {
		bad := filepath.Join(GinkgoT().TempDir(), "bad.json")
		Expect(os.WriteFile(bad, []byte("{"), 0o600)).To(Succeed())
		_, err := crosswalk.LoadCatalog(bad)
		Expect(err).To(MatchError(ContainSubstring(bad)))
	})
})

var _ = Describe("Catalog.Controls", func() {
	It("lists every control depth-first in document order", func() {
		c, err := crosswalk.LoadCatalog(syntheticCatalog)
		Expect(err).NotTo(HaveOccurred())
		var ids []string
		for _, ctrl := range c.Controls() {
			ids = append(ids, ctrl.ID)
		}
		Expect(ids).To(Equal([]string{"ab-2", "ab-2.3", "ab-2.3.1", "cd-1", "cd-9", "top.1"}))
	})
})

var _ = Describe("TargetIndex", func() {
	var x *crosswalk.TargetIndex

	BeforeEach(func() {
		c, err := crosswalk.LoadCatalog(syntheticCatalog)
		Expect(err).NotTo(HaveOccurred())
		x = crosswalk.NewTargetIndex(c)
	})

	DescribeTable("resolves a unique identifier",
		func(identifier, want string) {
			got := x.Resolve(identifier)
			Expect(got.State).To(Equal(crosswalk.Resolved))
			Expect(got.ControlID).To(Equal(want))
			Expect(got.Candidates).To(BeEmpty())
		},
		Entry("by ID", "ab-2.3", "ab-2.3"),
		Entry("by ID in another case", "AB-2.3", "ab-2.3"),
		Entry("by padded label", "AB-02(03)", "ab-2.3"),
		Entry("by unpadded label", "ab-2(3)", "ab-2.3"),
		Entry("top-level control by label", "TOP-01", "top.1"),
		Entry("grandchild by ID", "ab-2.3.1", "ab-2.3.1"),
		Entry("an ID and its own label share a form", "ab-2", "ab-2"),
	)

	It("leaves an unknown identifier unresolved", func() {
		got := x.Resolve("zz-99")
		Expect(got.State).To(Equal(crosswalk.Unresolved))
		Expect(got.ControlID).To(BeEmpty())
	})

	It("leaves an empty or separator-only identifier unresolved", func() {
		Expect(x.Resolve("").State).To(Equal(crosswalk.Unresolved))
		Expect(x.Resolve("()").State).To(Equal(crosswalk.Unresolved))
	})

	It("reports two controls sharing a canonical label as ambiguous and lists both", func() {
		got := x.Resolve("SHARED-9")
		Expect(got.State).To(Equal(crosswalk.Ambiguous))
		Expect(got.ControlID).To(BeEmpty())
		Expect(got.Candidates).To(ConsistOf("cd-1", "cd-9"))
	})

	DescribeTable("Base returns the outermost ancestor control",
		func(id, want string) {
			Expect(x.Base(id)).To(Equal(want))
		},
		Entry("grandchild", "ab-2.3.1", "ab-2"),
		Entry("child", "ab-2.3", "ab-2"),
		Entry("top-level control in a group is its own base", "ab-2", "ab-2"),
		Entry("control outside any group is its own base", "top.1", "top.1"),
		Entry("unknown control", "nope", ""),
	)

	It("reports which control IDs it holds", func() {
		Expect(x.Has("ab-2.3")).To(BeTrue())
		Expect(x.Has("nope")).To(BeFalse())
	})
})

var _ = Describe("TargetIndex statement items", func() {
	var x *crosswalk.TargetIndex

	BeforeEach(func() {
		c, err := crosswalk.LoadCatalog(statementCatalog)
		Expect(err).NotTo(HaveOccurred())
		x = crosswalk.NewTargetIndex(c)
	})

	// sa-1 items follow the "<id>_smt.<x>" part-ID pattern; np-1 items do not,
	// so the importer numbers them.
	DescribeTable("knows the item IDs the importer derives, rolling each up to its outermost control",
		func(id, wantBase string) {
			Expect(x.Has(id)).To(BeTrue())
			Expect(x.Base(id)).To(Equal(wantBase))
		},
		Entry("item under a control", "sa-1.a", "sa-1"),
		Entry("sub-item under an item", "sa-1.b.1", "sa-1"),
		Entry("item under an enhancement", "sa-1.2.a", "sa-1"),
		Entry("item with a part ID outside the pattern", "np-1.1", "np-1"),
		Entry("second such item", "np-1.2", "np-1"),
		Entry("the control itself", "sa-1", "sa-1"),
		Entry("an enhancement", "sa-1.2", "sa-1"),
		Entry("a control without a statement", "pl-9", "pl-9"),
	)

	DescribeTable("does not know an ID that is neither a control nor a derived item",
		func(id string) {
			Expect(x.Has(id)).To(BeFalse())
			Expect(x.Base(id)).To(BeEmpty())
		},
		Entry("the raw part ID", "sa-1_smt.a"),
		Entry("an item of no control", "sa-1.z"),
		Entry("a fallback index past the items", "np-1.3"),
	)

	It("does not resolve an item identifier, so references stay on controls", func() {
		Expect(x.Resolve("sa-1.a").State).To(Equal(crosswalk.Unresolved))
		Expect(x.Resolve("sa-1").State).To(Equal(crosswalk.Resolved))
	})
})

var _ = Describe("TargetIndex.Owner", func() {
	var x *crosswalk.TargetIndex

	BeforeEach(func() {
		x = crosswalk.NewTargetIndex(mustCatalog(statementCatalog))
	})

	DescribeTable("returns the nearest control containing an ID",
		func(id, want string) {
			Expect(x.Owner(id)).To(Equal(want))
		},
		Entry("a control", "sa-1", "sa-1"),
		Entry("an enhancement", "sa-1.2", "sa-1.2"),
		Entry("an item of a control", "sa-1.a", "sa-1"),
		Entry("a sub-item", "sa-1.b.1", "sa-1"),
		Entry("an item of an enhancement", "sa-1.2.a", "sa-1.2"),
		Entry("an item with a part ID outside the pattern", "np-1.2", "np-1"),
		Entry("an unknown ID", "nope", ""),
	)
})
