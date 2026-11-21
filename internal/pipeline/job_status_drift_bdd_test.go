package pipeline_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
)

// internal/catalog may not import this package, so it keeps its own copy of
// the terminal job statuses. This is the drift check: pipeline is the
// composition root and may import both.
var _ = Describe("catalog retirement's view of job statuses", func() {
	DescribeTable("agrees with pipeline on whether a job is finished",
		func(status pipeline.JobStatus, terminal bool) {
			Expect(catalog.IsTerminalJobStatus(string(status))).To(Equal(terminal))
		},
		Entry("completed is terminal", pipeline.JobStatusCompleted, true),
		Entry("failed is terminal", pipeline.JobStatusFailed, true),
		Entry("cancelled is terminal", pipeline.JobStatusCancelled, true),
		Entry("pending may still read catalogs", pipeline.JobStatusPending, false),
		Entry("running may still read catalogs", pipeline.JobStatusRunning, false),
	)

	It("treats a status it does not know as in flight", func() {
		Expect(catalog.IsTerminalJobStatus("paused")).To(BeFalse())
	})
})
