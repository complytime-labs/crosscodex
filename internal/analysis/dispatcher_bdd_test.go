package analysis_test

import (
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// fakeNATSClient records publishes for test assertions.
type fakeNATSClient struct {
	mu         sync.Mutex
	published  []fakePublish
	publishErr error

	natsbus.Client // embed to satisfy interface; unused methods panic
}

type fakePublish struct {
	subject string
	data    []byte
	headers map[string][]string
}

func (f *fakeNATSClient) Publish(_ context.Context, subject string, data []byte) error {
	return f.PublishWithHeaders(context.Background(), subject, data, nil)
}

func (f *fakeNATSClient) PublishWithHeaders(_ context.Context, subject string, data []byte, headers map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return f.publishErr
	}
	f.published = append(f.published, fakePublish{subject: subject, data: data, headers: headers})
	return nil
}

func (f *fakeNATSClient) Close() error { return nil }

var _ = Describe("NATSDispatcher", func() {
	var (
		fake       *fakeNATSClient
		dispatcher *analysis.NATSDispatcher
		ctx        context.Context
	)

	BeforeEach(func() {
		fake = &fakeNATSClient{}
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
			Expect(fake.published).To(HaveLen(2))

			Expect(fake.published[0].headers[analysis.ExportHeaderTaskID]).To(Equal([]string{"t-1"}))
			Expect(fake.published[0].headers[analysis.ExportHeaderRetryCount]).To(Equal([]string{"0"}))
			Expect(fake.published[1].headers[analysis.ExportHeaderTaskID]).To(Equal([]string{"t-2"}))
		})

		It("returns error when tenant context is missing", func() {
			err := dispatcher.Dispatch(context.Background(), []analyzer.Task{{TaskID: "t-1"}}, natsbus.TaskClassify, "job-1")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analysis.ErrNoTenant)).To(BeTrue())
		})

		It("wraps publish errors with task context", func() {
			fake.publishErr = errors.New("connection lost")
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
			Expect(fake.published[0].headers[analysis.ExportHeaderRetryCount]).To(Equal([]string{"2"}))
		})
	})
})
