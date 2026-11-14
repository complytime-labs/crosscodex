package crosswalk_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var _ = Describe("EquivalenceNotes", func() {
	row := func(srcNode, srcCat, srcID, tgtNode, tgtCat, tgtID string, typ pb.RelationshipType, dir crosswalk.Direction) *pb.CrosswalkRow {
		return &pb.CrosswalkRow{
			Source:           &pb.CrosswalkSide{NodeId: srcNode, CatalogId: srcCat, Identifier: srcID},
			Target:           &pb.CrosswalkSide{NodeId: tgtNode, CatalogId: tgtCat, Identifier: tgtID},
			RelationshipType: typ,
			Direction:        string(dir),
		}
	}
	eq := func(srcNode, srcCat, srcID, tgtNode, tgtCat, tgtID string) *pb.CrosswalkRow {
		return row(srcNode, srcCat, srcID, tgtNode, tgtCat, tgtID, equivalent, crosswalk.Direct)
	}

	It("returns an empty aligned slice for no rows", func() {
		Expect(crosswalk.EquivalenceNotes(nil)).To(BeEmpty())
	})

	It("flags one source equivalent to several targets in one catalog", func() {
		notes := crosswalk.EquivalenceNotes([]*pb.CrosswalkRow{
			eq("s1", "ism", "ism-1657", "t2", "nist", "cm-7.5.b"),
			eq("s1", "ism", "ism-1657", "t1", "nist", "cm-7.4.b"),
		})
		note := "one of 2 'equivalent' targets for ism-1657: cm-7.4.b, cm-7.5.b"
		Expect(notes).To(Equal([]string{note, note}))
	})

	It("flags several sources equivalent to one target", func() {
		notes := crosswalk.EquivalenceNotes([]*pb.CrosswalkRow{
			eq("s2", "ism", "b-1", "t1", "nist", "cm-7"),
			eq("s1", "ism", "a-1", "t1", "nist", "cm-7"),
		})
		note := "one of 2 'equivalent' sources for cm-7: a-1, b-1"
		Expect(notes).To(Equal([]string{note, note}))
	})

	It("gives both notes, source note first, to a row in both kinds of group", func() {
		notes := crosswalk.EquivalenceNotes([]*pb.CrosswalkRow{
			eq("s1", "ism", "a-1", "t1", "nist", "x-1"),
			eq("s1", "ism", "a-1", "t2", "nist", "x-2"),
			eq("s2", "ism", "a-2", "t1", "nist", "x-1"),
		})
		Expect(notes[0]).To(Equal("one of 2 'equivalent' targets for a-1: x-1, x-2; one of 2 'equivalent' sources for x-1: a-1, a-2"))
		Expect(notes[1]).To(Equal("one of 2 'equivalent' targets for a-1: x-1, x-2"))
		Expect(notes[2]).To(Equal("one of 2 'equivalent' sources for x-1: a-1, a-2"))
	})

	It("lists both nodes when two distinct nodes share an identifier", func() {
		notes := crosswalk.EquivalenceNotes([]*pb.CrosswalkRow{
			eq("s1", "ism", "a-1", "t1", "nist", "x-1"),
			eq("s1", "ism", "a-1", "t2", "nist", "x-1"),
		})
		Expect(notes[0]).To(Equal("one of 2 'equivalent' targets for a-1: x-1, x-1"))
	})

	DescribeTable("rows that get no note",
		func(rows []*pb.CrosswalkRow) {
			Expect(crosswalk.EquivalenceNotes(rows)).To(Equal(make([]string, len(rows))))
		},
		Entry("one-to-one equivalent", []*pb.CrosswalkRow{
			eq("s1", "ism", "a-1", "t1", "nist", "x-1"),
		}),
		Entry("one equivalent target in each of two target catalogs", []*pb.CrosswalkRow{
			eq("s1", "ism", "a-1", "t1", "nist", "x-1"),
			eq("s1", "ism", "a-1", "t2", "iso", "y-1"),
		}),
		Entry("partial rows to the same catalog", []*pb.CrosswalkRow{
			row("s1", "ism", "a-1", "t1", "nist", "x-1", partial, crosswalk.Direct),
			row("s1", "ism", "a-1", "t2", "nist", "x-2", partial, crosswalk.Direct),
		}),
		Entry("derived equivalent rows", []*pb.CrosswalkRow{
			row("s1", "ism", "a-1", "t1", "nist", "x-1", equivalent, crosswalk.Derived),
			row("s1", "ism", "a-1", "t2", "nist", "x-2", equivalent, crosswalk.Derived),
		}),
		Entry("one equivalent row beside a partial row", []*pb.CrosswalkRow{
			eq("s1", "ism", "a-1", "t1", "nist", "x-1"),
			row("s1", "ism", "a-1", "t2", "nist", "x-2", partial, crosswalk.Direct),
		}),
	)
})
