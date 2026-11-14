package crosswalk_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var _ = Describe("Merge", func() {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	verdict := func(job string, at time.Time, typ pb.RelationshipType, conf float64, reversed bool) crosswalk.Verdict {
		return crosswalk.Verdict{JobID: job, CompletedAt: at, Type: typ, Confidence: conf, Reversed: reversed}
	}

	It("returns the zero value for no verdicts", func() {
		Expect(crosswalk.Merge(nil)).To(Equal(crosswalk.Merged{}))
	})

	It("lets the latest completion time win", func() {
		m := crosswalk.Merge([]crosswalk.Verdict{
			verdict("job-new", t0.Add(time.Hour), partial, 0.7, false),
			verdict("job-old", t0, equivalent, 0.9, false),
		})
		Expect(m.Type).To(Equal(partial))
		Expect(m.Confidence).To(Equal(0.7))
		Expect(m.LatestJob).To(Equal("job-new"))
		Expect(m.Direction).To(Equal(crosswalk.Direct))
		Expect(m.JobsJudged).To(Equal(2))
		Expect(m.JobsAgreeing).To(Equal(1))
		Expect(m.Others).To(HaveLen(1))
		Expect(m.Others[0].JobID).To(Equal("job-old"))
	})

	It("breaks a time tie with the greater job ID", func() {
		m := crosswalk.Merge([]crosswalk.Verdict{
			verdict("job-a", t0, equivalent, 0.9, false),
			verdict("job-b", t0, partial, 0.5, false),
		})
		Expect(m.LatestJob).To(Equal("job-b"))
		Expect(m.Type).To(Equal(partial))
	})

	It("counts a reversed subset as agreeing with a forward superset", func() {
		m := crosswalk.Merge([]crosswalk.Verdict{
			verdict("job-1", t0, superset, 0.8, false),
			verdict("job-2", t0.Add(time.Hour), subset, 0.6, true),
		})
		Expect(m.Type).To(Equal(superset))
		Expect(m.Direction).To(Equal(crosswalk.Direct))
		Expect(m.JobsJudged).To(Equal(2))
		Expect(m.JobsAgreeing).To(Equal(2))
	})

	It("keeps a reversed contributes-to as a reverse row that never agrees", func() {
		m := crosswalk.Merge([]crosswalk.Verdict{
			verdict("job-1", t0, contributes, 0.8, false),
			verdict("job-2", t0.Add(time.Hour), contributes, 0.6, true),
		})
		Expect(m.Type).To(Equal(contributes))
		Expect(m.Direction).To(Equal(crosswalk.Reverse))
		Expect(m.LatestJob).To(Equal("job-2"))
		Expect(m.JobsJudged).To(Equal(2))
		Expect(m.JobsAgreeing).To(Equal(1))
		Expect(m.Others).To(HaveLen(1))
		Expect(m.Others[0].Reversed).To(BeFalse())
	})

	It("counts a bidirectional job's two verdicts as one job", func() {
		m := crosswalk.Merge([]crosswalk.Verdict{
			verdict("job-1", t0, subset, 0.8, false),
			verdict("job-1", t0, partial, 0.6, true),
		})
		Expect(m.JobsJudged).To(Equal(1))
		Expect(m.JobsAgreeing).To(Equal(1))
		Expect(m.Others).To(HaveLen(1))
	})

	It("lets a job's higher-confidence direction decide the row and keeps the other as a dissent", func() {
		forward := verdict("job-1", t0, subset, 0.6, false)
		reverse := verdict("job-1", t0, conflicts, 0.9, true)
		for _, vs := range [][]crosswalk.Verdict{{forward, reverse}, {reverse, forward}} {
			m := crosswalk.Merge(vs)
			Expect(m.Type).To(Equal(conflicts))
			Expect(m.Confidence).To(Equal(0.9))
			Expect(m.JobsJudged).To(Equal(1))
			Expect(m.JobsAgreeing).To(Equal(1))
			Expect(m.Others).To(HaveLen(1))
			Expect(m.Others[0].Type).To(Equal(subset))
		}
	})
})

var _ = Describe("FormatOthers", func() {
	It("renders word, confidence and the short job ID", func() {
		got := crosswalk.FormatOthers([]crosswalk.Verdict{
			{JobID: "0123456789abcdef", Type: subset, Confidence: 0.5},
			{JobID: "short", Type: contributes, Confidence: 0.25, Reversed: true},
		})
		Expect(got).To(Equal("subset of@0.50 (01234567); reverse: is advanced by@0.25 (short)"))
	})

	It("is empty for no verdicts", func() {
		Expect(crosswalk.FormatOthers(nil)).To(BeEmpty())
	})
})
