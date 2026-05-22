package natsbus

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// assertSubjectResult checks a (subject, error) pair against expected values.
// Shared by all subject-builder test tables.
func assertSubjectResult(t *testing.T, got string, err error, want string, wantErr error) {
	t.Helper()
	if wantErr != nil {
		if err == nil {
			t.Fatalf("expected error wrapping %v, got nil", wantErr)
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want wrapping %v", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPipelineStageSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tenant  string
		jobID   string
		stage   Stage
		want    string
		wantErr error
	}{
		{
			name:   "valid started",
			tenant: "acme-corp",
			jobID:  "job-001",
			stage:  StageStarted,
			want:   "crosscodex.pipeline.acme-corp.job-001.stage.started",
		},
		{
			name:   "valid completed",
			tenant: "acme-corp",
			jobID:  "job-001",
			stage:  StageCompleted,
			want:   "crosscodex.pipeline.acme-corp.job-001.stage.completed",
		},
		{
			name:   "valid failed",
			tenant: "acme-corp",
			jobID:  "job-001",
			stage:  StageFailed,
			want:   "crosscodex.pipeline.acme-corp.job-001.stage.failed",
		},
		{
			name:    "empty tenant",
			tenant:  "",
			jobID:   "job-001",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "invalid tenant",
			tenant:  "UPPERCASE",
			jobID:   "job-001",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "empty job ID",
			tenant:  "acme-corp",
			jobID:   "",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "job ID with dot",
			tenant:  "acme-corp",
			jobID:   "job.001",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "job ID with wildcard star",
			tenant:  "acme-corp",
			jobID:   "job*001",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "job ID with wildcard gt",
			tenant:  "acme-corp",
			jobID:   "job>001",
			stage:   StageStarted,
			wantErr: ErrInvalidSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := PipelineStageSubject(tt.tenant, tt.jobID, tt.stage)
			assertSubjectResult(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestPipelineStateSubject(t *testing.T) {
	t.Parallel()

	got, err := PipelineStateSubject("acme-corp", "job-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "crosscodex.pipeline.acme-corp.job-001.state"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorkSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tenant   string
		taskType TaskType
		jobID    string
		want     string
		wantErr  error
	}{
		{
			name:     "classify",
			tenant:   "acme-corp",
			taskType: TaskClassify,
			jobID:    "job-001",
			want:     "crosscodex.work.acme-corp.classify.job-001",
		},
		{
			name:     "relate",
			tenant:   "acme-corp",
			taskType: TaskRelate,
			jobID:    "job-001",
			want:     "crosscodex.work.acme-corp.relate.job-001",
		},
		{
			name:     "requires",
			tenant:   "acme-corp",
			taskType: TaskRequires,
			jobID:    "job-001",
			want:     "crosscodex.work.acme-corp.requires.job-001",
		},
		{
			name:     "artifacts",
			tenant:   "acme-corp",
			taskType: TaskArtifacts,
			jobID:    "job-001",
			want:     "crosscodex.work.acme-corp.artifacts.job-001",
		},
		{
			name:     "embed",
			tenant:   "acme-corp",
			taskType: TaskEmbed,
			jobID:    "job-001",
			want:     "crosscodex.work.acme-corp.embed.job-001",
		},
		{
			name:     "invalid tenant",
			tenant:   "BAD",
			taskType: TaskClassify,
			jobID:    "job-001",
			wantErr:  ErrInvalidSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := WorkSubject(tt.tenant, tt.taskType, tt.jobID)
			assertSubjectResult(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestResultSubject(t *testing.T) {
	t.Parallel()

	got, err := ResultSubject("acme-corp", TaskClassify, "job-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "crosscodex.results.acme-corp.classify.job-001"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAuditSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tenant    string
		auditType AuditType
		jobID     string
		want      string
		wantErr   error
	}{
		{
			name:      "llm audit",
			tenant:    "acme-corp",
			auditType: AuditLLM,
			jobID:     "job-001",
			want:      "crosscodex.audit.acme-corp.llm.job-001",
		},
		{
			name:      "decisions audit",
			tenant:    "acme-corp",
			auditType: AuditDecisions,
			jobID:     "job-001",
			want:      "crosscodex.audit.acme-corp.decisions.job-001",
		},
		{
			name:      "events audit",
			tenant:    "acme-corp",
			auditType: AuditEvents,
			jobID:     "job-001",
			want:      "crosscodex.audit.acme-corp.events.job-001",
		},
		{
			name:      "invalid tenant",
			tenant:    "x",
			auditType: AuditLLM,
			jobID:     "job-001",
			wantErr:   ErrInvalidSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := AuditSubject(tt.tenant, tt.auditType, tt.jobID)
			assertSubjectResult(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestFeedbackSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tenant  string
		edgeID  string
		want    string
		wantErr error
	}{
		{
			name:   "valid",
			tenant: "acme-corp",
			edgeID: "edge-abc-123",
			want:   "crosscodex.feedback.acme-corp.edge-abc-123",
		},
		{
			name:    "empty edge ID",
			tenant:  "acme-corp",
			edgeID:  "",
			wantErr: ErrInvalidSubject,
		},
		{
			name:    "edge ID with dot",
			tenant:  "acme-corp",
			edgeID:  "edge.bad",
			wantErr: ErrInvalidSubject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := FeedbackSubject(tt.tenant, tt.edgeID)
			assertSubjectResult(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestInjectProvenance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	data := []byte(`{"control":"AC-1","mapping":"full"}`)

	headers := injectProvenance(ctx, data, "test-tenant")

	// X-Tenant-Id
	if got := headers["X-Tenant-Id"]; len(got) != 1 || got[0] != "test-tenant" {
		t.Errorf("X-Tenant-Id = %v, want [test-tenant]", got)
	}

	// X-Content-SHA256
	h := sha256.Sum256(data)
	wantHash := hex.EncodeToString(h[:])
	if got := headers["X-Content-SHA256"]; len(got) != 1 || got[0] != wantHash {
		t.Errorf("X-Content-SHA256 = %v, want [%s]", got, wantHash)
	}

	// X-Timestamp must be valid RFC3339Nano
	if got := headers["X-Timestamp"]; len(got) != 1 {
		t.Fatalf("X-Timestamp missing")
	} else if _, err := time.Parse(time.RFC3339Nano, got[0]); err != nil {
		t.Errorf("X-Timestamp %q is not valid RFC3339Nano: %v", got[0], err)
	}

	// X-Trace-Id and X-Span-Id should be present (empty string OK without active span)
	if _, ok := headers["X-Trace-Id"]; !ok {
		t.Error("X-Trace-Id header missing")
	}
	if _, ok := headers["X-Span-Id"]; !ok {
		t.Error("X-Span-Id header missing")
	}
}

func TestInjectProvenanceDeterministicHash(t *testing.T) {
	t.Parallel()

	data := []byte("deterministic content")
	ctx := context.Background()

	h1 := injectProvenance(ctx, data, "tenant-aaa")
	h2 := injectProvenance(ctx, data, "tenant-aaa")

	if h1["X-Content-SHA256"][0] != h2["X-Content-SHA256"][0] {
		t.Errorf("content hash not deterministic: %q != %q",
			h1["X-Content-SHA256"][0], h2["X-Content-SHA256"][0])
	}
}

func TestExtractProvenance(t *testing.T) {
	t.Parallel()

	data := []byte("test payload")
	h := sha256.Sum256(data)
	wantHash := hex.EncodeToString(h[:])
	ts := time.Now().UTC()

	headers := map[string][]string{
		"X-Trace-Id":       {"abc123"},
		"X-Span-Id":        {"span456"},
		"X-Tenant-Id":      {"acme-corp"},
		"X-Timestamp":      {ts.Format(time.RFC3339Nano)},
		"X-Content-SHA256": {wantHash},
	}

	meta, err := extractProvenance(headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.TraceID != "abc123" {
		t.Errorf("TraceID = %q, want %q", meta.TraceID, "abc123")
	}
	if meta.SpanID != "span456" {
		t.Errorf("SpanID = %q, want %q", meta.SpanID, "span456")
	}
	if meta.TenantID != "acme-corp" {
		t.Errorf("TenantID = %q, want %q", meta.TenantID, "acme-corp")
	}
	if meta.ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q", meta.ContentHash, wantHash)
	}
}

func TestExtractProvenanceMissingHeaders(t *testing.T) {
	t.Parallel()

	_, err := extractProvenance(map[string][]string{})
	if err == nil {
		t.Fatal("expected error for empty headers, got nil")
	}
	if !errors.Is(err, ErrMissingProvenance) {
		t.Errorf("error = %v, want wrapping %v", err, ErrMissingProvenance)
	}

	// Error message must list all missing fields to be actionable.
	errMsg := err.Error()
	for _, field := range []string{"X-Tenant-Id", "X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"} {
		if !strings.Contains(errMsg, field) {
			t.Errorf("error message %q does not mention missing field %q", errMsg, field)
		}
	}
}

func TestExtractProvenancePartialHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		headers        map[string][]string
		wantMissing    []string
		wantNotMissing []string
	}{
		{
			name: "only tenant present",
			headers: map[string][]string{
				"X-Tenant-Id": {"acme-corp"},
			},
			wantMissing:    []string{"X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
			wantNotMissing: []string{"X-Tenant-Id"},
		},
		{
			name: "everything except tenant",
			headers: map[string][]string{
				"X-Trace-Id":       {"trace-1"},
				"X-Span-Id":        {"span-1"},
				"X-Timestamp":      {"2026-01-01T00:00:00Z"},
				"X-Content-SHA256": {"abc123"},
			},
			wantMissing:    []string{"X-Tenant-Id"},
			wantNotMissing: []string{"X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
		},
		{
			name:           "nil headers map",
			headers:        nil,
			wantMissing:    []string{"X-Tenant-Id", "X-Content-SHA256", "X-Timestamp", "X-Trace-Id", "X-Span-Id"},
			wantNotMissing: nil,
		},
		{
			name: "empty string values treated as missing",
			headers: map[string][]string{
				"X-Tenant-Id":      {""},
				"X-Trace-Id":       {""},
				"X-Span-Id":        {"valid-span"},
				"X-Timestamp":      {"2026-01-01T00:00:00Z"},
				"X-Content-SHA256": {"abc123"},
			},
			wantMissing:    []string{"X-Tenant-Id", "X-Trace-Id"},
			wantNotMissing: []string{"X-Span-Id", "X-Timestamp", "X-Content-SHA256"},
		},
		{
			name: "empty slice values treated as missing",
			headers: map[string][]string{
				"X-Tenant-Id":      {},
				"X-Trace-Id":       {"valid-trace"},
				"X-Span-Id":        {"valid-span"},
				"X-Timestamp":      {"2026-01-01T00:00:00Z"},
				"X-Content-SHA256": {"abc123"},
			},
			wantMissing:    []string{"X-Tenant-Id"},
			wantNotMissing: []string{"X-Trace-Id", "X-Span-Id", "X-Timestamp", "X-Content-SHA256"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := extractProvenance(tt.headers)
			if err == nil {
				t.Fatal("expected error for partial/invalid headers, got nil")
			}
			if !errors.Is(err, ErrMissingProvenance) {
				t.Errorf("error = %v, want wrapping %v", err, ErrMissingProvenance)
			}

			errMsg := err.Error()
			for _, field := range tt.wantMissing {
				if !strings.Contains(errMsg, field) {
					t.Errorf("error message %q does not mention missing field %q", errMsg, field)
				}
			}
			for _, field := range tt.wantNotMissing {
				if strings.Contains(errMsg, field) {
					t.Errorf("error message %q should not mention %q (it was provided)", errMsg, field)
				}
			}
		})
	}
}

func TestExtractProvenanceMultipleValues(t *testing.T) {
	t.Parallel()

	// When a header has multiple values, only the first is used.
	headers := map[string][]string{
		"X-Trace-Id":       {"first-trace", "second-trace"},
		"X-Span-Id":        {"first-span", "second-span"},
		"X-Tenant-Id":      {"first-tenant", "second-tenant"},
		"X-Timestamp":      {"2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"},
		"X-Content-SHA256": {"hash1", "hash2"},
	}

	meta, err := extractProvenance(headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.TraceID != "first-trace" {
		t.Errorf("TraceID = %q, want %q (should use first value)", meta.TraceID, "first-trace")
	}
	if meta.TenantID != "first-tenant" {
		t.Errorf("TenantID = %q, want %q (should use first value)", meta.TenantID, "first-tenant")
	}
}

func TestMergeHeaders(t *testing.T) {
	t.Parallel()

	base := map[string][]string{
		"X-Custom":    {"value1"},
		"X-Tenant-Id": {"should-be-overwritten"},
	}

	provenance := map[string][]string{
		"X-Tenant-Id": {"correct-tenant"},
		"X-Trace-Id":  {"trace-001"},
	}

	merged := mergeHeaders(base, provenance)

	if got := merged["X-Custom"]; len(got) != 1 || got[0] != "value1" {
		t.Errorf("X-Custom = %v, want [value1]", got)
	}
	if got := merged["X-Tenant-Id"]; len(got) != 1 || got[0] != "correct-tenant" {
		t.Errorf("X-Tenant-Id = %v, want [correct-tenant] (provenance takes precedence)", got)
	}
	if got := merged["X-Trace-Id"]; len(got) != 1 || got[0] != "trace-001" {
		t.Errorf("X-Trace-Id = %v, want [trace-001]", got)
	}
}

func TestXDGStateHome(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv which is incompatible with parallel execution.

	tests := []struct {
		name    string
		envVar  string
		wantEnd string
	}{
		{
			name:    "default fallback",
			envVar:  "",
			wantEnd: filepath.Join(".local", "state", "crosscodex", "nats"),
		},
		{
			name:    "custom XDG_STATE_HOME",
			envVar:  "/custom/state",
			wantEnd: filepath.Join("crosscodex", "nats"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envVar != "" {
				t.Setenv("XDG_STATE_HOME", tt.envVar)
			} else {
				t.Setenv("XDG_STATE_HOME", "")
			}

			got := xdgNATSStateDir()
			if !strings.HasSuffix(got, tt.wantEnd) {
				t.Errorf("xdgNATSStateDir() = %q, want suffix %q", got, tt.wantEnd)
			}
			if tt.envVar != "" && !strings.HasPrefix(got, tt.envVar) {
				t.Errorf("xdgNATSStateDir() = %q, want prefix %q", got, tt.envVar)
			}
		})
	}
}

func TestDefaultClientOptions(t *testing.T) {
	t.Parallel()

	opts := defaultClientOptions()

	if opts.connectTimeout != 5*time.Second {
		t.Errorf("connectTimeout = %v, want 5s", opts.connectTimeout)
	}
	if opts.reconnectWait != 2*time.Second {
		t.Errorf("reconnectWait = %v, want 2s", opts.reconnectWait)
	}
	if opts.maxReconnects != 60 {
		t.Errorf("maxReconnects = %d, want 60", opts.maxReconnects)
	}
	if opts.logger == nil {
		t.Error("logger should not be nil")
	}
	if opts.tlsConfig != nil {
		t.Error("tlsConfig should be nil by default")
	}
}

func TestWithOptions(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	opts := defaultClientOptions()
	for _, o := range []Option{
		WithLogger(logger),
		WithConnectTimeout(10 * time.Second),
		WithReconnectWait(5 * time.Second),
		WithMaxReconnects(100),
		WithTLSConfig(tlsCfg),
	} {
		o(&opts)
	}

	if opts.connectTimeout != 10*time.Second {
		t.Errorf("connectTimeout = %v, want 10s", opts.connectTimeout)
	}
	if opts.reconnectWait != 5*time.Second {
		t.Errorf("reconnectWait = %v, want 5s", opts.reconnectWait)
	}
	if opts.maxReconnects != 100 {
		t.Errorf("maxReconnects = %d, want 100", opts.maxReconnects)
	}
	if opts.logger != logger {
		t.Error("logger not set correctly")
	}
	if opts.tlsConfig != tlsCfg {
		t.Error("tlsConfig not set correctly")
	}
}

func TestResolveStoreDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured string
		wantCustom bool
	}{
		{
			name:       "uses configured dir",
			configured: "/tmp/nats-test-store",
			wantCustom: true,
		},
		{
			name:       "falls back to XDG",
			configured: "",
			wantCustom: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.NATSEmbeddedConfig{
				StoreDir: tt.configured,
			}
			got := resolveStoreDir(cfg)
			if tt.wantCustom && got != tt.configured {
				t.Errorf("resolveStoreDir() = %q, want %q", got, tt.configured)
			}
			if !tt.wantCustom && got == "" {
				t.Error("resolveStoreDir() should not return empty when unconfigured")
			}
		})
	}
}

func TestIsEmbeddedMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "empty URL is embedded", url: "", want: true},
		{name: "non-empty URL is external", url: "nats://localhost:4222", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isEmbeddedMode(tt.url); got != tt.want {
				t.Errorf("isEmbeddedMode(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestAuditStreamConfigs(t *testing.T) {
	t.Parallel()

	streamsCfg := config.NATSStreamsConfig{
		AuditLLMRetention:    2160 * time.Hour,
		AuditEventsRetention: 720 * time.Hour,
	}

	configs := auditStreamConfigs(streamsCfg)

	if len(configs) != 3 {
		t.Fatalf("expected 3 stream configs, got %d", len(configs))
	}

	tests := []struct {
		name     string
		wantName string
		wantSubj string
		wantAge  time.Duration
	}{
		{
			name:     "AUDIT_LLM",
			wantName: "AUDIT_LLM",
			wantSubj: "crosscodex.audit.*.llm.>",
			wantAge:  2160 * time.Hour,
		},
		{
			name:     "AUDIT_DECISIONS",
			wantName: "AUDIT_DECISIONS",
			wantSubj: "crosscodex.audit.*.decisions.>",
			wantAge:  0, // indefinite
		},
		{
			name:     "AUDIT_EVENTS",
			wantName: "AUDIT_EVENTS",
			wantSubj: "crosscodex.audit.*.events.>",
			wantAge:  720 * time.Hour,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := configs[i]
			if cfg.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", cfg.Name, tt.wantName)
			}
			if len(cfg.Subjects) != 1 || cfg.Subjects[0] != tt.wantSubj {
				t.Errorf("Subjects = %v, want [%s]", cfg.Subjects, tt.wantSubj)
			}
			if cfg.MaxAge != tt.wantAge {
				t.Errorf("MaxAge = %v, want %v", cfg.MaxAge, tt.wantAge)
			}
		})
	}
}
