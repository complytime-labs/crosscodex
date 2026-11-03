package worker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
)

// FuzzExtractCompletionRequest feeds arbitrary bytes to the proto + payload
// extraction path that processes raw NATS task payloads. It must never panic.
func FuzzExtractCompletionRequest(f *testing.F) {
	// Seed: valid proto-encoded Struct with messages field.
	validPayload, _ := structpb.NewStruct(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are a classifier."},
			map[string]interface{}{"role": "user", "content": "Classify this."},
		},
		"model":          "gpt-4",
		"temperature":    0.7,
		"max_tokens":     float64(256),
		"prompt_name":    "classify",
		"prompt_version": "1.0",
		"content_hash":   "abc123",
	})
	if validBytes, err := proto.Marshal(validPayload); err == nil {
		f.Add(validBytes)
	}

	// Seed: empty bytes.
	f.Add([]byte{})
	// Seed: random garbage.
	f.Add([]byte{0x00, 0xFF, 0xAB, 0x01, 0x02, 0x03})
	// Seed: truncated proto.
	f.Add([]byte{0x0a, 0x05})
	// Seed: messages field with invalid role.
	badRole, _ := structpb.NewStruct(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "INJECTED", "content": "evil"},
		},
	})
	if badRoleBytes, err := proto.Marshal(badRole); err == nil {
		f.Add(badRoleBytes)
	}
	// Seed: messages field with scalar instead of struct.
	scalarMsg, _ := structpb.NewStruct(map[string]interface{}{
		"messages": []interface{}{"not-a-struct"},
	})
	if scalarBytes, err := proto.Marshal(scalarMsg); err == nil {
		f.Add(scalarBytes)
	}
	// Seed: content_hash mismatch.
	mismatch, _ := structpb.NewStruct(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
		"content_hash": "definitely-wrong-hash",
	})
	if mismatchBytes, err := proto.Marshal(mismatch); err == nil {
		f.Add(mismatchBytes)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		payload := &structpb.Struct{}
		if err := proto.Unmarshal(raw, payload); err != nil {
			// Non-proto input: expected; not a crash.
			return
		}
		// Must not panic regardless of payload content.
		_, _ = extractCompletionRequest(payload, "tenant-fuzz", "job-fuzz")
	})
}

// FuzzExtractEmbeddingRequest feeds arbitrary bytes to the embedding extraction
// path. It must never panic.
func FuzzExtractEmbeddingRequest(f *testing.F) {
	validPayload, _ := structpb.NewStruct(map[string]interface{}{
		"text":  "compliance requirement",
		"model": "text-embedding-3-small",
	})
	if validBytes, err := proto.Marshal(validPayload); err == nil {
		f.Add(validBytes)
	}

	f.Add([]byte{})
	f.Add([]byte{0x00, 0xFF, 0xAB})

	f.Fuzz(func(t *testing.T, raw []byte) {
		payload := &structpb.Struct{}
		if err := proto.Unmarshal(raw, payload); err != nil {
			return
		}
		_, _ = extractEmbeddingRequest(payload, "tenant-fuzz", "job-fuzz")
	})
}

// FuzzHandleAbandoned feeds arbitrary control messages to the worker's
// abandon handler, the deserialization point for messages anyone on the bus
// can publish. It must never panic or fail the subscription, and it may
// record an abandon only for a well-formed, payload-free message whose
// provenance tenant is the subject's tenant.
func FuzzHandleAbandoned(f *testing.F) {
	valid := "crosscodex.control.tenant-a.relate.job-1.0b7e7a9e-5f6c-4a51-9d7e-3c1f2b8a4d10.abandoned"
	f.Add(valid, "tenant-a", []byte{})
	f.Add(valid, "tenant-b", []byte{})
	f.Add(valid, "", []byte{})
	f.Add(valid, "tenant-a", []byte("task content"))
	f.Add("crosscodex.control.BAD.relate.job-1.c-1.abandoned", "BAD", []byte{})
	f.Add("crosscodex.control.tenant-a.relate.*.>.abandoned", "tenant-a", []byte{})
	f.Add("", "", []byte{0x00})

	f.Fuzz(func(t *testing.T, subject, tenantID string, data []byte) {
		w := New(&natsbustest.Fake{}, nil, WorkerConfig{MaxAbandonedCollections: 10}, WithAbandonTTL(time.Hour),
			WithLogger(slog.New(slog.DiscardHandler)))
		msg := &natsbus.Message{Subject: subject, Data: data, Metadata: natsbus.MessageMetadata{TenantID: tenantID}}
		if err := w.handleAbandoned(context.Background(), msg); err != nil {
			t.Fatalf("handleAbandoned returned %v; it must always return nil", err)
		}
		if w.abandoned.len() == 0 {
			return
		}
		c, err := natsbus.ParseAbandonedSubject(subject)
		if err != nil {
			t.Fatalf("recorded an abandon for unparseable subject %q: %v", subject, err)
		}
		if c.TenantID != tenantID || len(data) != 0 {
			t.Fatalf("recorded an abandon for %q with provenance tenant %q and %d payload bytes", subject, tenantID, len(data))
		}
		if !w.abandoned.contains(c) {
			t.Fatalf("recorded set does not hold %+v", c)
		}
	})
}
