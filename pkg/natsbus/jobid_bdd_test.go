package natsbus_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("Job ID validation in subjects", func() {
	const collection = "0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10"
	longest := strings.Repeat("j", natsbus.MaxJobIDLength)
	tooLong := longest + "j"

	// Every builder that puts a job ID into a subject.
	builders := map[string]func(jobID string) (string, error){
		"PipelineStageSubject": func(id string) (string, error) {
			return natsbus.PipelineStageSubject("acme-corp", id, natsbus.StageStarted)
		},
		"PipelineStateSubject": func(id string) (string, error) { return natsbus.PipelineStateSubject("acme-corp", id) },
		"WorkSubject":          func(id string) (string, error) { return natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, id) },
		"ResultSubject":        func(id string) (string, error) { return natsbus.ResultSubject("acme-corp", natsbus.TaskRelate, id) },
		"AuditSubject":         func(id string) (string, error) { return natsbus.AuditSubject("acme-corp", natsbus.AuditLLM, id) },
		"AbandonedSubject": func(id string) (string, error) {
			return natsbus.AbandonedSubject(natsbus.AbandonedCollection{
				TenantID: "acme-corp", TaskType: natsbus.TaskRelate, JobID: id, CollectionID: collection,
			})
		},
	}

	It("caps job IDs at 128 bytes", func() {
		Expect(natsbus.MaxJobIDLength).To(Equal(128))
	})

	DescribeTable("every job-ID subject builder accepts a job ID of exactly the maximum length",
		func(builder string) {
			got, err := builders[builder](longest)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(ContainSubstring("." + longest + "."))
		},
		Entry("PipelineStageSubject", "PipelineStageSubject"),
		Entry("PipelineStateSubject", "PipelineStateSubject"),
		Entry("AbandonedSubject", "AbandonedSubject"),
	)

	DescribeTable("every job-ID subject builder accepts a job ID of exactly the maximum length as its last token",
		func(builder string) {
			got, err := builders[builder](longest)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveSuffix("." + longest))
		},
		Entry("WorkSubject", "WorkSubject"),
		Entry("ResultSubject", "ResultSubject"),
		Entry("AuditSubject", "AuditSubject"),
	)

	DescribeTable("every job-ID subject builder rejects an invalid job ID with ErrInvalidJobID and ErrInvalidSubject",
		func(builder, jobID, wantMsg string) {
			got, err := builders[builder](jobID)
			Expect(err).To(MatchError(natsbus.ErrInvalidJobID))
			Expect(err).To(MatchError(natsbus.ErrInvalidSubject))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			Expect(got).To(BeEmpty())
		},
		Entry("PipelineStageSubject, one byte too long", "PipelineStageSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("PipelineStateSubject, one byte too long", "PipelineStateSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("WorkSubject, one byte too long", "WorkSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("ResultSubject, one byte too long", "ResultSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("AuditSubject, one byte too long", "AuditSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("AbandonedSubject, one byte too long", "AbandonedSubject", tooLong, "job ID is 129 bytes, longer than the 128-byte limit"),
		Entry("too long, with guidance", "WorkSubject", tooLong, "use the job ID crosscodexd assigned"),
		Entry("WorkSubject, empty", "WorkSubject", "", "job ID must not be empty"),
		Entry("AuditSubject, with a delimiter", "AuditSubject", "job.1", "contains NATS subject delimiter"),
		Entry("AbandonedSubject, with a wildcard", "AbandonedSubject", "job*", "contains NATS subject delimiter"),
	)

	It("does not quote an over-long job ID back in the error", func() {
		_, err := natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, tooLong)
		Expect(err).To(MatchError(natsbus.ErrInvalidJobID))
		Expect(err.Error()).NotTo(ContainSubstring(tooLong))
	})

	DescribeTable("ParseAbandonedSubject applies the same job ID cap",
		func(jobID string, wantErr bool) {
			subject := "crosscodex.control.acme-corp.relate." + jobID + "." + collection + ".abandoned"
			c, err := natsbus.ParseAbandonedSubject(subject)
			if wantErr {
				Expect(err).To(MatchError(natsbus.ErrInvalidJobID))
				Expect(err).To(MatchError(natsbus.ErrInvalidSubject))
				Expect(c).To(BeZero())
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(c.JobID).To(Equal(jobID))
		},
		Entry("accepts a 128-byte job ID", longest, false),
		Entry("rejects a 129-byte job ID", tooLong, true),
	)
})
