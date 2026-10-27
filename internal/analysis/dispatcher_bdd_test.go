package analysis_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("NATSDispatcher", func() {
	var (
		fake       *natsbustest.Fake
		dispatcher *analysis.NATSDispatcher
		ctx        context.Context
	)

	BeforeEach(func() {
		fake = &natsbustest.Fake{}
		dispatcher = analysis.NewNATSDispatcher(fake)
		ctx, _ = tenant.WithTenant(context.Background(), "test-tenant")
	})

	Describe("Dispatch", func() {
		It("publishes each task to the correct subject with headers", func() {
			payload, _ := structpb.NewStruct(map[string]interface{}{"key": "val"})
			tasks := []analyzer.Task{
				{TaskID: "t-1", TaskType: "classify", Payload: payload},
				{TaskID: "t-2", TaskType: "classify", Payload: payload},
			}

			err := dispatcher.Dispatch(ctx, tasks, natsbus.TaskClassify, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.Published()).To(HaveLen(2))

			Expect(fake.Published()[0].Headers[analysis.ExportHeaderTaskID]).To(Equal([]string{"t-1"}))
			Expect(fake.Published()[0].Headers[analysis.ExportHeaderRetryCount]).To(Equal([]string{"0"}))
			Expect(fake.Published()[1].Headers[analysis.ExportHeaderTaskID]).To(Equal([]string{"t-2"}))
		})

		It("writes the routing task type into the header, not the task's own analyzer name", func() {
			// The task's TaskType field carries the analyzer name ("embedding"),
			// but the worker switches on the natsbus routing constant. The header
			// must carry the routing type ("embed"), or the worker rejects it as
			// unsupported_task_type.
			payload, _ := structpb.NewStruct(map[string]interface{}{"key": "val"})
			tasks := []analyzer.Task{{TaskID: "t-1", TaskType: "embedding", Payload: payload}}

			err := dispatcher.Dispatch(ctx, tasks, natsbus.TaskEmbed, "job-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.Published()).To(HaveLen(1))
			Expect(fake.Published()[0].Headers[analysis.ExportHeaderTaskType]).To(Equal([]string{string(natsbus.TaskEmbed)}))
		})

		It("returns error when tenant context is missing", func() {
			err := dispatcher.Dispatch(context.Background(), []analyzer.Task{{TaskID: "t-1"}}, natsbus.TaskClassify, "job-1")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analysis.ErrNoTenant)).To(BeTrue())
			Expect(fake.Published()).To(BeEmpty())
		})

		It("wraps publish errors with task context", func() {
			fake.PublishWithHeadersFunc = func(context.Context, string, []byte, map[string][]string) error {
				return errors.New("connection lost")
			}
			payload, _ := structpb.NewStruct(map[string]interface{}{})
			tasks := []analyzer.Task{{TaskID: "t-1", TaskType: "classify", Payload: payload}}

			err := dispatcher.Dispatch(ctx, tasks, natsbus.TaskClassify, "job-1")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("t-1"))
			Expect(err.Error()).To(ContainSubstring("connection lost"))
		})
	})

	Describe("Redispatch", func() {
		It("sets the retry count header", func() {
			payload, _ := structpb.NewStruct(map[string]interface{}{})
			task := analyzer.Task{TaskID: "t-1", TaskType: "classify", Payload: payload}

			err := dispatcher.Redispatch(ctx, task, natsbus.TaskClassify, "job-1", 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.Published()[0].Headers[analysis.ExportHeaderRetryCount]).To(Equal([]string{"2"}))
		})
	})
})
