package natsbus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// fakeStream is a minimal jetstream.Stream whose Info returns a configured
// MaxAge (or a configured error). Unused methods are inherited from the
// embedded nil interface and would panic if called.
type fakeStream struct {
	jetstream.Stream
	maxAge  time.Duration
	infoErr error
}

func (s *fakeStream) Info(_ context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return &jetstream.StreamInfo{Config: jetstream.StreamConfig{MaxAge: s.maxAge}}, nil
}

// fakeJetStream is a minimal jetstream.JetStream: only Stream is exercised.
// Unused methods are inherited from the embedded nil interface.
type fakeJetStream struct {
	jetstream.JetStream
	streams   map[string]*fakeStream
	streamErr map[string]error
}

func (f *fakeJetStream) Stream(_ context.Context, name string) (jetstream.Stream, error) {
	if err, ok := f.streamErr[name]; ok {
		return nil, err
	}
	s, ok := f.streams[name]
	if !ok {
		return nil, jetstream.ErrStreamNotFound
	}
	return s, nil
}

func TestAuditStreamRetentionAllPresent(t *testing.T) {
	c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
		"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
		"AUDIT_DECISIONS": {maxAge: 0},
		"AUDIT_EVENTS":    {maxAge: 30 * 24 * time.Hour},
	}}}

	live, err := c.AuditStreamRetention(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(live) != 3 {
		t.Fatalf("expected 3 streams, got %d: %v", len(live), live)
	}
	if live["AUDIT_LLM"] != 90*24*time.Hour {
		t.Errorf("AUDIT_LLM: want 90d, got %v", live["AUDIT_LLM"])
	}
	if live["AUDIT_EVENTS"] != 30*24*time.Hour {
		t.Errorf("AUDIT_EVENTS: want 30d, got %v", live["AUDIT_EVENTS"])
	}
}

func TestAuditStreamRetentionNotFoundOmitted(t *testing.T) {
	// AUDIT_EVENTS is absent (Stream returns ErrStreamNotFound); it must be
	// omitted from the map, NOT surfaced as an error, so VerifyStreamRetention
	// reports it as Live=0/OK=false.
	c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
		"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
		"AUDIT_DECISIONS": {maxAge: 0},
	}}}

	live, err := c.AuditStreamRetention(context.Background())
	if err != nil {
		t.Fatalf("not-found stream must not error, got: %v", err)
	}
	if _, present := live["AUDIT_EVENTS"]; present {
		t.Errorf("absent stream AUDIT_EVENTS must be omitted, got %v", live)
	}
	if len(live) != 2 {
		t.Fatalf("expected 2 present streams, got %d: %v", len(live), live)
	}
}

func TestAuditStreamRetentionInfoNotFoundOmitted(t *testing.T) {
	// A stream that exists at lookup but reports not-found on Info (deleted
	// concurrently) is likewise omitted rather than failing the read.
	c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
		"AUDIT_LLM":       {maxAge: 90 * 24 * time.Hour},
		"AUDIT_DECISIONS": {maxAge: 0},
		"AUDIT_EVENTS":    {infoErr: jetstream.ErrStreamNotFound},
	}}}

	live, err := c.AuditStreamRetention(context.Background())
	if err != nil {
		t.Fatalf("info not-found must not error, got: %v", err)
	}
	if _, present := live["AUDIT_EVENTS"]; present {
		t.Errorf("AUDIT_EVENTS must be omitted, got %v", live)
	}
}

func TestAuditStreamRetentionGenuineStreamErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	c := &client{js: &fakeJetStream{
		streams:   map[string]*fakeStream{"AUDIT_DECISIONS": {maxAge: 0}, "AUDIT_EVENTS": {maxAge: 0}},
		streamErr: map[string]error{"AUDIT_LLM": boom},
	}}

	live, err := c.AuditStreamRetention(context.Background())
	if err == nil {
		t.Fatalf("genuine Stream error must propagate, got live=%v", live)
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected wrapped boom, got: %v", err)
	}
	if live != nil {
		t.Errorf("expected nil map on error, got %v", live)
	}
}

func TestAuditStreamRetentionGenuineInfoErrorPropagates(t *testing.T) {
	boom := errors.New("request timeout")
	c := &client{js: &fakeJetStream{streams: map[string]*fakeStream{
		"AUDIT_LLM":       {infoErr: boom},
		"AUDIT_DECISIONS": {maxAge: 0},
		"AUDIT_EVENTS":    {maxAge: 0},
	}}}

	live, err := c.AuditStreamRetention(context.Background())
	if err == nil {
		t.Fatalf("genuine Info error must propagate, got live=%v", live)
	}
	if !errors.Is(err, boom) {
		t.Errorf("expected wrapped boom, got: %v", err)
	}
}
