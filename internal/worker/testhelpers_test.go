package worker_test

import (
	"context"
	"time"

	"github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// BuildCompletionPayload creates a structpb payload for a completion work task.
func BuildCompletionPayload(promptName, model string, temperature float64, maxTokens int) *structpb.Struct {
	messages := []llmclient.ChatMessage{
		{Role: "system", Content: "You are a test."},
		{Role: "user", Content: "Test input."},
	}
	payload, err := structpb.NewStruct(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": messages[0].Role, "content": messages[0].Content},
			map[string]interface{}{"role": messages[1].Role, "content": messages[1].Content},
		},
		"model":          model,
		"temperature":    temperature,
		"max_tokens":     float64(maxTokens),
		"prompt_name":    promptName,
		"prompt_version": "1.0",
		"content_hash":   llmclient.ContentHash(messages),
	})
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	return payload
}

// BuildEmbeddingPayload creates a structpb payload for an embedding work task.
func BuildEmbeddingPayload(model, text string) *structpb.Struct {
	payload, err := structpb.NewStruct(map[string]interface{}{
		"text":       text,
		"model":      model,
		"batch_size": float64(100),
	})
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	return payload
}

// PublishWorkTask publishes a work task to NATS with the standard headers.
func PublishWorkTask(ctx context.Context, bus natsbus.Client, tenantID string, taskType natsbus.TaskType, jobID, taskID string, payload *structpb.Struct) {
	subject, err := natsbus.WorkSubject(tenantID, taskType, jobID)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	data, err := proto.Marshal(payload)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	headers := map[string][]string{
		"X-Task-Id":     {taskID},
		"X-Task-Type":   {string(taskType)},
		"X-Job-Id":      {jobID},
		"X-Retry-Count": {"0"},
	}

	err = bus.PublishWithHeaders(ctx, subject, data, headers)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
}

// WaitForResult subscribes to the result subject and waits for a result
// matching the given task ID, or returns nil on timeout.
func WaitForResult(ctx context.Context, bus natsbus.Client, tenantID string, taskType natsbus.TaskType, jobID, taskID string, timeout time.Duration) *structpb.Struct {
	subject, err := natsbus.ResultSubject(tenantID, taskType, jobID)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	resultCh := make(chan *structpb.Struct, 1)
	sub, err := bus.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
		if vals := msg.Headers["X-Task-Id"]; len(vals) > 0 && vals[0] == taskID {
			if errVals := msg.Headers["X-Error"]; len(errVals) > 0 {
				return nil
			}
			s := &structpb.Struct{}
			if err := proto.Unmarshal(msg.Data, s); err == nil {
				select {
				case resultCh <- s:
				default:
				}
			}
		}
		return nil
	})
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	defer func() { gomega.ExpectWithOffset(1, sub.Unsubscribe()).To(gomega.Succeed()) }()

	select {
	case r := <-resultCh:
		return r
	case <-time.After(timeout):
		return nil
	}
}

// WaitForErrorResult subscribes to the result subject and waits for an error
// result (X-Error header present) matching the given task ID. Returns the
// error category string, or empty string on timeout.
func WaitForErrorResult(ctx context.Context, bus natsbus.Client, tenantID string, taskType natsbus.TaskType, jobID, taskID string, timeout time.Duration) string {
	subject, err := natsbus.ResultSubject(tenantID, taskType, jobID)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	errCh := make(chan string, 1)
	sub, err := bus.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
		if vals := msg.Headers["X-Task-Id"]; len(vals) > 0 && vals[0] == taskID {
			if errVals := msg.Headers["X-Error"]; len(errVals) > 0 {
				select {
				case errCh <- errVals[0]:
				default:
				}
			}
		}
		return nil
	})
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	defer func() { gomega.ExpectWithOffset(1, sub.Unsubscribe()).To(gomega.Succeed()) }()

	select {
	case e := <-errCh:
		return e
	case <-time.After(timeout):
		return ""
	}
}
