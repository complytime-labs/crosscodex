package crosswalk_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var _ = Describe("Collapse", func() {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := func(hub string, typ pb.RelationshipType, conf float64, jobs ...string) crosswalk.Path {
		return crosswalk.Path{Hub: hub, Type: typ, Confidence: conf, Jobs: jobs, LatestJob: jobs[0], LatestAt: t0}
	}

	It("returns the zero value for no paths", func() {
		Expect(crosswalk.Collapse(nil)).To(Equal(crosswalk.Collapsed{}))
	})

	DescribeTable("verdict, support and notes",
		func(paths []crosswalk.Path, typ pb.RelationshipType, conf float64, supporting int, notes []string) {
			c := crosswalk.Collapse(paths)
			Expect(c.Type).To(Equal(typ))
			Expect(c.Confidence).To(Equal(conf))
			Expect(c.PathsSupporting).To(Equal(supporting))
			Expect(c.PathsTotal).To(Equal(len(paths)))
			Expect(c.Notes).To(Equal(notes))
		},
		Entry("two agreeing paths keep the max confidence and add no note",
			[]crosswalk.Path{path("h1", subset, 0.9, "j1"), path("h2", subset, 0.6, "j1")},
			subset, 0.9, 2, nil),
		Entry("one named of two counts only named paths and adds no rule 3 note",
			[]crosswalk.Path{path("h1", subset, 0.5, "j1"), path("h2", unspecified, 0.9, "j1")},
			subset, 0.5, 1, nil),
		Entry("one named of three adds the rule 3 note",
			[]crosswalk.Path{path("h1", subset, 0.5, "j1"), path("h2", unspecified, 0.9, "j1"), path("h3", unspecified, 0.8, "j1")},
			subset, 0.5, 1, []string{"'subset of' rests on 1 of 3 paths"}),
		Entry("disagreeing named paths read type unknown with a note sorted by hub",
			[]crosswalk.Path{path("ia-2.1", superset, 0.4, "j1"), path("ac-6.5", subset, 0.7, "j1")},
			unspecified, 0.7, 2, []string{"paths disagree: subset of via ac-6.5, superset of via ia-2.1"}),
		Entry("all unknown paths read type unknown without a note",
			[]crosswalk.Path{path("h1", unspecified, 0.3, "j1"), path("h2", unspecified, 0.6, "j1")},
			unspecified, 0.6, 2, nil),
		Entry("disagreement keeps unnamed paths as supporting",
			[]crosswalk.Path{path("b", subset, 0.4, "j1"), path("a", superset, 0.5, "j1"), path("c", unspecified, 0.9, "j1")},
			unspecified, 0.9, 3, []string{"paths disagree: superset of via a, subset of via b"}),
	)

	It("lists distinct sorted hubs and jobs, counting only supporting jobs as agreeing", func() {
		c := crosswalk.Collapse([]crosswalk.Path{
			path("h2", subset, 0.5, "j1", "j2"),
			path("h1", subset, 0.5, "j2", "j3"),
			path("h1", unspecified, 0.5, "j9"),
		})
		Expect(c.Hubs).To(Equal([]string{"h1", "h2"}))
		Expect(c.Jobs).To(Equal([]string{"j1", "j2", "j3", "j9"}))
		Expect(c.JobsAgreeing).To(Equal(3))
	})

	It("counts every job as agreeing when the type is unknown", func() {
		c := crosswalk.Collapse([]crosswalk.Path{
			path("h1", unspecified, 0.5, "j1"),
			path("h2", unspecified, 0.5, "j2"),
		})
		Expect(c.JobsAgreeing).To(Equal(len(c.Jobs)))
	})

	DescribeTable("latest job among supporting paths",
		func(paths []crosswalk.Path, want string) {
			Expect(crosswalk.Collapse(paths).LatestJob).To(Equal(want))
		},
		Entry("newest completion time wins",
			[]crosswalk.Path{
				{Hub: "h1", Type: subset, Jobs: []string{"j1"}, LatestJob: "j1", LatestAt: t0},
				{Hub: "h2", Type: subset, Jobs: []string{"j0"}, LatestJob: "j0", LatestAt: t0.Add(time.Hour)},
			}, "j0"),
		Entry("a time tie goes to the greater job ID",
			[]crosswalk.Path{
				{Hub: "h1", Type: subset, Jobs: []string{"j2"}, LatestJob: "j2", LatestAt: t0},
				{Hub: "h2", Type: subset, Jobs: []string{"j1"}, LatestJob: "j1", LatestAt: t0},
			}, "j2"),
		Entry("a newer unsupporting path is ignored",
			[]crosswalk.Path{
				{Hub: "h1", Type: subset, Jobs: []string{"j1"}, LatestJob: "j1", LatestAt: t0},
				{Hub: "h2", Type: unspecified, Jobs: []string{"j9"}, LatestJob: "j9", LatestAt: t0.Add(time.Hour)},
			}, "j1"),
	)
})
