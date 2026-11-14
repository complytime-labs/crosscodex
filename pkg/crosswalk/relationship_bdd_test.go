package crosswalk_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

const (
	unspecified = pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
	equivalent  = pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT
	superset    = pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF
	subset      = pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF
	contributes = pb.RelationshipType_RELATIONSHIP_TYPE_CONTRIBUTES_TO
	complements = pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS
	partial     = pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL
	conflicts   = pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH
	none        = pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP
	outOfRange  = pb.RelationshipType(99)
)

var _ = Describe("Relationship rules", func() {
	DescribeTable("Word",
		func(t pb.RelationshipType, want string) {
			Expect(crosswalk.Word(t)).To(Equal(want))
		},
		Entry("equivalent", equivalent, "equivalent"),
		Entry("subset", subset, "subset of"),
		Entry("superset", superset, "superset of"),
		Entry("contributes", contributes, "is advanced by"),
		Entry("complements", complements, "complements"),
		Entry("partial", partial, "partial"),
		Entry("conflicts", conflicts, "conflicts with"),
		Entry("none", none, "no relationship"),
		Entry("unspecified falls back to the enum name", unspecified, "unspecified"),
		Entry("out of range falls back to the number", outOfRange, "99"),
	)

	DescribeTable("Invert",
		func(t, want pb.RelationshipType, wantOK bool) {
			got, ok := crosswalk.Invert(t)
			Expect(got).To(Equal(want))
			Expect(ok).To(Equal(wantOK))
		},
		Entry("unspecified", unspecified, unspecified, false),
		Entry("equivalent", equivalent, equivalent, true),
		Entry("superset becomes subset", superset, subset, true),
		Entry("subset becomes superset", subset, superset, true),
		Entry("contributes has no inverse", contributes, contributes, false),
		Entry("complements", complements, complements, true),
		Entry("partial", partial, partial, true),
		Entry("conflicts", conflicts, conflicts, true),
		Entry("none", none, none, true),
		Entry("out of range", outOfRange, outOfRange, false),
	)

	DescribeTable("Compose",
		func(ab, bc, want pb.RelationshipType, wantOK bool) {
			got, ok := crosswalk.Compose(ab, bc)
			Expect(got).To(Equal(want))
			Expect(ok).To(Equal(wantOK))
		},
		Entry("equivalent then X", equivalent, contributes, contributes, true),
		Entry("X then equivalent", partial, equivalent, partial, true),
		Entry("equivalent then equivalent", equivalent, equivalent, equivalent, true),
		Entry("subset then subset", subset, subset, subset, true),
		Entry("superset then superset", superset, superset, superset, true),
		Entry("subset then superset is unknown", subset, superset, unspecified, false),
		Entry("superset then subset is unknown", superset, subset, unspecified, false),
		Entry("contributes then subset is unknown", contributes, subset, unspecified, false),
		Entry("none on first leg", none, equivalent, none, false),
		Entry("none on second leg", subset, none, none, false),
		Entry("equivalent then unspecified is unknown", equivalent, unspecified, unspecified, false),
		Entry("out of range is unknown", outOfRange, equivalent, unspecified, false),
	)
})
