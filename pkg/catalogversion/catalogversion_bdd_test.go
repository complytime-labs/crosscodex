package catalogversion_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/catalogversion"
)

var _ = Describe("Catalog version display", func() {
	importedAt := time.Date(2026, 3, 9, 23, 30, 0, 0, time.FixedZone("west", -5*3600))
	const hash = "0123456789abcdef"

	It("formats slug, label, UTC date and hash prefix", func() {
		Expect(catalogversion.Format("nist-800-53", "5.2.0", importedAt, hash)).
			To(Equal("nist-800-53 @ 5.2.0 (2026-03-10 · 012345)"))
	})

	DescribeTable("DateAndHash",
		func(contentHash, want string) {
			Expect(catalogversion.DateAndHash(importedAt, contentHash)).To(Equal(want))
		},
		Entry("keeps the first six characters of a long hash", hash, "2026-03-10 · 012345"),
		Entry("uses a short hash as-is", "abc", "2026-03-10 · abc"),
		Entry("leaves the hash part empty for no hash", "", "2026-03-10 · "),
	)

	DescribeTable("OfCatalog",
		func(cat *pb.Catalog, want string) {
			Expect(catalogversion.OfCatalog(cat)).To(Equal(want))
		},
		Entry("renders a document version from its stored fields",
			&pb.Catalog{
				DocumentSlug: "nist-800-53", VersionLabel: "Rev 5",
				Audit:      &pb.AuditMetadata{CreatedAt: timestamppb.New(importedAt)},
				Provenance: &pb.ProvenanceMetadata{LineageMetadata: map[string]string{"content_hash": hash}},
			},
			"nist-800-53 @ Rev 5 (2026-03-10 · 012345)"),
		Entry("is empty for a catalog outside any document",
			&pb.Catalog{VersionLabel: "ignored", Audit: &pb.AuditMetadata{CreatedAt: timestamppb.New(importedAt)}}, ""),
		Entry("is empty for no catalog", nil, ""),
	)
})
