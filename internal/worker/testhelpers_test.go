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

// WatchResult subscribes to the result subject for taskID and returns a
// function that waits up to timeout for that task's success result, returning
// nil on timeout. Call it BEFORE publishing the task: the bus is core NATS,
// which drops a message nobody is subscribed to, so a subscription made after
// the publish loses the result whenever the worker answers first. The returned
// function removes the subscription before it returns; a spec that never calls
// it leaves the subscription to the connection close in AfterEach.
func WatchResult(ctx context.Context, bus natsbus.Client, tenantID string, taskType natsbus.TaskType, jobID, taskID string) func(time.Duration) *structpb.Struct {
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

	return func(timeout time.Duration) *structpb.Struct {
		defer func() { gomega.ExpectWithOffset(1, sub.Unsubscribe()).To(gomega.Succeed()) }()

		select {
		case r := <-resultCh:
			return r
		case <-time.After(timeout):
			return nil
		}
	}
}

// WatchErrorResult subscribes to the result subject for taskID and returns a
// function that waits up to timeout for that task's error result (X-Error
// header present), returning the error category or "" on timeout. Call it
// BEFORE publishing the task, for the reason given on WatchResult. The
// subscription is removed as described on WatchResult.
func WatchErrorResult(ctx context.Context, bus natsbus.Client, tenantID string, taskType natsbus.TaskType, jobID, taskID string) func(time.Duration) string {
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

	return func(timeout time.Duration) string {
		defer func() { gomega.ExpectWithOffset(1, sub.Unsubscribe()).To(gomega.Succeed()) }()

		select {
		case e := <-errCh:
			return e
		case <-time.After(timeout):
			return ""
		}
	}
}
