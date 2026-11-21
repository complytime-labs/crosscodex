//go:build !integration

package catalog_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
)

var _ = Describe("Document slug and version label validation", func() {
	DescribeTable("ValidateDocumentSlug accepts",
		func(slug string) {
			Expect(catalog.ValidateDocumentSlug(slug)).To(Succeed())
		},
		Entry("a lowercase word", "nist-800-53"),
		Entry("a single character", "a"),
		Entry("a leading digit", "9x"),
		Entry("exactly 63 characters", strings.Repeat("a", 63)),
	)

	DescribeTable("ValidateDocumentSlug rejects",
		func(slug string) {
			err := catalog.ValidateDocumentSlug(slug)
			Expect(err).To(MatchError(catalog.ErrInvalidDocumentSlug))
			Expect(err.Error()).To(ContainSubstring("a-z0-9"))
		},
		Entry("the empty string", ""),
		Entry("uppercase letters", "NIST"),
		Entry("64 characters", strings.Repeat("a", 64)),
		Entry("a leading hyphen", "-abc"),
		Entry("a space", "a b"),
		Entry("a newline", "abc\n"),
		Entry("a path traversal", "../x"),
	)

	DescribeTable("ValidateVersionLabel accepts",
		func(label string) {
			Expect(catalog.ValidateVersionLabel(label)).To(Succeed())
		},
		Entry("a dotted version", "5.2.0"),
		Entry("spaces and a middle dot", "2026-01-02 · abc123"),
		Entry("exactly 64 characters", strings.Repeat("a", 64)),
		Entry("64 multi-byte characters", strings.Repeat("é", 64)),
	)

	DescribeTable("ValidateVersionLabel rejects",
		func(label string) {
			err := catalog.ValidateVersionLabel(label)
			Expect(err).To(MatchError(catalog.ErrInvalidVersionLabel))
			Expect(err.Error()).To(ContainSubstring("label"))
		},
		Entry("the empty string", ""),
		Entry("65 characters", strings.Repeat("a", 65)),
		Entry("a newline", "v1\nv2"),
		Entry("a tab", "v1\tv2"),
		Entry("a NUL byte", "v1\x00"),
		Entry("invalid UTF-8", "v1\xff"),
		Entry("a bidi override", "v1\u202e2"),
		Entry("a zero-width space", "v1\u200b2"),
		Entry("a line separator", "v1\u20282"),
		Entry("a paragraph separator", "v1\u20292"),
		Entry("a less-than sign", "v<script>"),
		Entry("a greater-than sign", "v1>"),
		Entry("a backtick", "v`1`"),
	)
})

var _ = Describe("Version label resolution", func() {
	importedAt := time.Date(2026, 3, 9, 23, 30, 0, 0, time.FixedZone("west", -5*3600))
	const hash = "0123456789abcdef"

	DescribeTable("ResolveVersionLabel",
		func(explicit, oscal, want string) {
			Expect(catalog.ResolveVersionLabel(explicit, oscal, importedAt, hash)).To(Equal(want))
		},
		Entry("prefers the explicit label", "rev5", "5.2.0", "rev5"),
		Entry("falls back to the OSCAL version", "", "5.2.0", "5.2.0"),
		Entry("falls back to UTC date and hash prefix", "", "", "2026-03-10 · 012345"),
	)

	It("uses a short content hash as-is in the fallback", func() {
		Expect(catalog.ResolveVersionLabel("", "", importedAt, "abc")).To(Equal("2026-03-10 · abc"))
	})
})

var _ = Describe("DiffControls", func() {
	ctl := func(id, statement string) catalog.ControlRecord {
		return catalog.ControlRecord{Identifier: id, Statement: statement}
	}

	DescribeTable("classifies changes between versions",
		func(prev, next []catalog.ControlRecord, added, removed, changed []string) {
			got := catalog.DiffControls(prev, next)
			Expect(got.Added).To(Equal(added))
			Expect(got.Removed).To(Equal(removed))
			Expect(got.Changed).To(Equal(changed))
			Expect(got.IDsUnstable).To(BeFalse())
		},
		Entry("an added control",
			[]catalog.ControlRecord{ctl("a", "x")},
			[]catalog.ControlRecord{ctl("a", "x"), ctl("b", "y")},
			[]string{"b"}, []string(nil), []string(nil)),
		Entry("a removed control",
			[]catalog.ControlRecord{ctl("a", "x"), ctl("b", "y")},
			[]catalog.ControlRecord{ctl("a", "x")},
			[]string(nil), []string{"b"}, []string(nil)),
		Entry("a changed statement",
			[]catalog.ControlRecord{ctl("a", "x"), ctl("b", "y")},
			[]catalog.ControlRecord{ctl("a", "x"), ctl("b", "y2")},
			[]string(nil), []string(nil), []string{"b"}),
		Entry("no change",
			[]catalog.ControlRecord{ctl("a", "x")},
			[]catalog.ControlRecord{ctl("a", "x")},
			[]string(nil), []string(nil), []string(nil)),
		Entry("sorted output",
			[]catalog.ControlRecord{ctl("m", "1"), ctl("z", "1")},
			[]catalog.ControlRecord{ctl("m", "1"), ctl("z", "1"), ctl("c", "1"), ctl("b", "1")},
			[]string{"b", "c"}, []string(nil), []string(nil)),
	)

	It("reports everything as added against an empty previous version, without flagging unstable ids", func() {
		got := catalog.DiffControls(nil, []catalog.ControlRecord{ctl("a", "x"), ctl("b", "y")})
		Expect(got.Added).To(Equal([]string{"a", "b"}))
		Expect(got.Removed).To(BeEmpty())
		Expect(got.Changed).To(BeEmpty())
		Expect(got.IDsUnstable).To(BeFalse())
	})

	It("flags unstable ids when fewer than 10% of ids are shared", func() {
		var prev, next []catalog.ControlRecord
		for _, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10", "shared"} {
			prev = append(prev, ctl(id, "x"))
		}
		for _, id := range []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9", "n10", "n11", "n12", "shared"} {
			next = append(next, ctl(id, "x"))
		}
		Expect(catalog.DiffControls(prev, next).IDsUnstable).To(BeTrue())
	})

	It("does not flag unstable ids at exactly 10% shared", func() {
		var prev, next []catalog.ControlRecord
		for _, id := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "shared"} {
			prev = append(prev, ctl(id, "x"))
		}
		for _, id := range []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9", "shared"} {
			next = append(next, ctl(id, "x"))
		}
		Expect(catalog.DiffControls(prev, next).IDsUnstable).To(BeFalse())
	})

	It("flags unstable ids for completely disjoint non-empty versions", func() {
		got := catalog.DiffControls(
			[]catalog.ControlRecord{ctl("a", "x")},
			[]catalog.ControlRecord{ctl("b", "x")})
		Expect(got.IDsUnstable).To(BeTrue())
	})

	It("does not flag unstable ids when the next version is empty", func() {
		got := catalog.DiffControls([]catalog.ControlRecord{ctl("a", "x")}, nil)
		Expect(got.Removed).To(Equal([]string{"a"}))
		Expect(got.IDsUnstable).To(BeFalse())
	})
})
