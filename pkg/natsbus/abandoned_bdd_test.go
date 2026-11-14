package natsbus_test

import (
	"strings"

	"github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("Abandoned collection control subject", func() {
	const collection = "0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10"
	valid := natsbus.AbandonedCollection{
		TenantID:     "acme-corp",
		TaskType:     natsbus.TaskRelate,
		JobID:        "job-001",
		CollectionID: "0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10",
	}
	const validSubject = "crosscodex.control.acme-corp.relate.job-001.0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned"

	It("builds crosscodex.control.{tenant}.{task_type}.{job}.{collection}.abandoned", func() {
		Expect(natsbus.AbandonedSubject(valid)).To(Equal(validSubject))
	})

	It("is matched by AbandonedSubjectFilter and by no work or result subject pattern", func() {
		Expect(server.SubjectMatchesFilter(validSubject, natsbus.AbandonedSubjectFilter)).To(BeTrue())
		Expect(server.SubjectMatchesFilter(validSubject, "crosscodex.work.>")).To(BeFalse())
		Expect(server.SubjectMatchesFilter(validSubject, "crosscodex.results.>")).To(BeFalse())
	})

	It("parses a subject it built back into the same collection", func() {
		Expect(natsbus.ParseAbandonedSubject(validSubject)).To(Equal(valid))
	})

	DescribeTable("rejects a collection whose fields cannot form the subject",
		func(mutate func(*natsbus.AbandonedCollection), wantMsg string) {
			c := valid
			mutate(&c)
			_, err := natsbus.AbandonedSubject(c)
			Expect(err).To(MatchError(natsbus.ErrInvalidSubject))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("invalid tenant", func(c *natsbus.AbandonedCollection) { c.TenantID = "BAD" }, "tenant validation"),
		Entry("empty task type", func(c *natsbus.AbandonedCollection) { c.TaskType = "" }, "task type must not be empty"),
		Entry("task type with a delimiter", func(c *natsbus.AbandonedCollection) { c.TaskType = "re.late" }, "task type"),
		Entry("unknown task type", func(c *natsbus.AbandonedCollection) { c.TaskType = "summarize" },
			`task type "summarize" is not one of classify, relate, requires, artifacts, embed, critique`),
		Entry("empty job ID", func(c *natsbus.AbandonedCollection) { c.JobID = "" }, "job ID must not be empty"),
		Entry("job ID with a wildcard", func(c *natsbus.AbandonedCollection) { c.JobID = "job*" }, "job ID"),
		Entry("empty collection ID", func(c *natsbus.AbandonedCollection) { c.CollectionID = "" }, "collection ID must not be empty"),
		Entry("collection ID with a wildcard", func(c *natsbus.AbandonedCollection) { c.CollectionID = ">" }, "collection ID"),
		Entry("collection ID that is not a UUID", func(c *natsbus.AbandonedCollection) { c.CollectionID = "coll-1" },
			`collection ID "coll-1" is not a lower-case RFC 4122 UUID`),
		Entry("upper-case collection UUID", func(c *natsbus.AbandonedCollection) { c.CollectionID = strings.ToUpper(c.CollectionID) },
			"is not a lower-case RFC 4122 UUID"),
		Entry("braced collection UUID", func(c *natsbus.AbandonedCollection) { c.CollectionID = "{" + c.CollectionID + "}" },
			"is not a lower-case RFC 4122 UUID"),
		Entry("collection UUID without dashes", func(c *natsbus.AbandonedCollection) {
			c.CollectionID = strings.ReplaceAll(c.CollectionID, "-", "")
		}, "is not a lower-case RFC 4122 UUID"),
	)

	DescribeTable("rejects a subject that is not an abandon control subject",
		func(subject, wantMsg string) {
			_, err := natsbus.ParseAbandonedSubject(subject)
			Expect(err).To(MatchError(natsbus.ErrInvalidSubject))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("empty subject", "", "crosscodex.control.<tenant_id>.<task_type>.<job_id>.<collection_id>.abandoned"),
		Entry("work subject", "crosscodex.work.acme-corp.relate.job-001", "crosscodex.control.<tenant_id>"),
		Entry("too few tokens", "crosscodex.control.acme-corp.relate.job-001.abandoned", "crosscodex.control.<tenant_id>"),
		Entry("too many tokens", "crosscodex.control.acme-corp.relate.job-001."+collection+".x.abandoned", "crosscodex.control.<tenant_id>"),
		Entry("wrong final token", "crosscodex.control.acme-corp.relate.job-001."+collection+".cancelled", "crosscodex.control.<tenant_id>"),
		Entry("wrong prefix", "other.control.acme-corp.relate.job-001."+collection+".abandoned", "crosscodex.control.<tenant_id>"),
		Entry("invalid tenant", "crosscodex.control.BAD.relate.job-001."+collection+".abandoned", "tenant validation"),
		Entry("empty task type", "crosscodex.control.acme-corp..job-001."+collection+".abandoned", "task type must not be empty"),
		Entry("unknown task type", "crosscodex.control.acme-corp.summarize.job-001."+collection+".abandoned", "is not one of"),
		Entry("wildcard job ID", "crosscodex.control.acme-corp.relate.*."+collection+".abandoned", "job ID"),
		Entry("wildcard collection ID", "crosscodex.control.acme-corp.relate.job-001.>.abandoned", "collection ID"),
		Entry("collection ID that is not a UUID", "crosscodex.control.acme-corp.relate.job-001.c-1.abandoned", "is not a lower-case RFC 4122 UUID"),
	)

})
