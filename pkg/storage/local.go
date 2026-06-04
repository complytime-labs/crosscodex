package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type localProvider struct {
	root     string // absolute path: {root}/{tenantID}/
	realRoot string // symlink-resolved version of root
	tenantID string
	closed   atomic.Bool

	// Telemetry (optional, nil-safe)
	tracer    trace.Tracer
	meter     metric.Meter
	opCounter metric.Int64Counter
	opLatency metric.Int64Histogram
}

// LocalOption configures a local storage provider.
type LocalOption func(*localProvider) error

// WithLocalTelemetry configures OpenTelemetry for the local provider.
func WithLocalTelemetry(tracer trace.Tracer, meter metric.Meter) LocalOption {
	return func(p *localProvider) error {
		p.tracer = tracer
		p.meter = meter
		var err error
		p.opCounter, err = meter.Int64Counter("storage.operations.total",
			metric.WithDescription("Total storage operations"))
		if err != nil {
			return fmt.Errorf("create operation counter: %w", err)
		}
		p.opLatency, err = meter.Int64Histogram("storage.operation.duration_ms",
			metric.WithDescription("Storage operation duration in milliseconds"))
		if err != nil {
			return fmt.Errorf("create operation latency histogram: %w", err)
		}
		return nil
	}
}

// NewLocal creates a Provider backed by the local filesystem.
// All operations are scoped to {root}/{tenantID}/.
func NewLocal(root, tenantID string, opts ...LocalOption) (Provider, error) {
	if err := validateTenantID(tenantID); err != nil {
		return nil, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving root path: %w", err)
	}

	tenantRoot := filepath.Join(absRoot, tenantID)
	if err := os.MkdirAll(tenantRoot, 0700); err != nil {
		return nil, fmt.Errorf("creating tenant directory: %w", err)
	}

	realRoot, err := filepath.EvalSymlinks(tenantRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving tenant root: %w", err)
	}

	p := &localProvider{
		root:     tenantRoot,
		realRoot: realRoot,
		tenantID: tenantID,
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, fmt.Errorf("apply local option: %w", err)
		}
	}
	return p, nil
}

func (p *localProvider) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if p.tracer != nil {
		return p.tracer.Start(ctx, name)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("storage").Start(ctx, name)
}

func (p *localProvider) resolveAndVerify(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}

	target := filepath.Join(p.root, filepath.Clean(key))

	// Walk up to the deepest existing ancestor to catch symlink escapes.
	existing := target
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}

	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}

	if !strings.HasPrefix(realExisting+string(filepath.Separator), p.realRoot+string(filepath.Separator)) &&
		realExisting != p.realRoot {
		p.logViolation(key, realExisting, "path escapes tenant root")
		return "", fmt.Errorf("%w: path escapes tenant boundary", ErrInvalidKey)
	}

	return target, nil
}

func (p *localProvider) logViolation(key, resolved, reason string) {
	slog.Error("storage access denied",
		"event", "storage.access_denied",
		"tenant_id", p.tenantID,
		"requested_key", key,
		"resolved_path", resolved,
		"reason", reason,
	)
}

func (p *localProvider) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.Get")
	defer span.End()
	span.SetAttributes(attribute.String("storage.key", key))

	path, err := p.resolveAndVerify(key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			span.SetStatus(codes.Error, ErrNotFound.Error())
			return nil, ErrNotFound
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("opening file: %w", err)
	}

	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return f, nil
}

func (p *localProvider) Put(ctx context.Context, key string, data io.Reader) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.Put")
	defer span.End()
	span.SetAttributes(attribute.String("storage.key", key))

	path, err := p.resolveAndVerify(key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("creating directories: %w", err)
	}

	// Re-verify after creating directories to close TOCTOU window.
	if _, err := p.resolveAndVerify(key); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()

	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, data); err != nil {
		_ = tmp.Close()
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("writing data: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("renaming temp file: %w", err)
	}

	success = true
	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p *localProvider) Delete(ctx context.Context, key string) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.Delete")
	defer span.End()
	span.SetAttributes(attribute.String("storage.key", key))

	path, err := p.resolveAndVerify(key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("removing file: %w", err)
	}

	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (p *localProvider) List(ctx context.Context, prefix string) ([]ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.List")
	defer span.End()
	span.SetAttributes(attribute.String("storage.prefix", prefix))

	searchRoot := p.root
	if prefix != "" {
		if err := validateKey(prefix); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		searchRoot = filepath.Join(p.root, filepath.Clean(prefix))
	}

	var result []ObjectMetadata
	err := filepath.WalkDir(searchRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(p.root, path)
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		result = append(result, ObjectMetadata{
			Key:          rel,
			Size:         info.Size(),
			LastModified: info.ModTime().Unix(),
		})
		return nil
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("walking directory: %w", err)
	}

	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return result, nil
}

func (p *localProvider) Exists(ctx context.Context, key string) (bool, error) {
	if p.closed.Load() {
		return false, ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.Exists")
	defer span.End()
	span.SetAttributes(attribute.String("storage.key", key))

	path, err := p.resolveAndVerify(key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return false, err
	}

	_, err = os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if p.opCounter != nil {
				p.opCounter.Add(ctx, 1)
			}
			if p.opLatency != nil {
				p.opLatency.Record(ctx, time.Since(start).Milliseconds())
			}
			span.SetStatus(codes.Ok, "")
			return false, nil
		}
		span.SetStatus(codes.Error, err.Error())
		return false, fmt.Errorf("stat: %w", err)
	}

	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return true, nil
}

func (p *localProvider) Stat(ctx context.Context, key string) (*ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}

	start := time.Now()
	ctx, span := p.startSpan(ctx, "storage.Stat")
	defer span.End()
	span.SetAttributes(attribute.String("storage.key", key))

	path, err := p.resolveAndVerify(key)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			span.SetStatus(codes.Error, ErrNotFound.Error())
			return nil, ErrNotFound
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("stat: %w", err)
	}

	if p.opCounter != nil {
		p.opCounter.Add(ctx, 1)
	}
	if p.opLatency != nil {
		p.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return &ObjectMetadata{
		Key:          key,
		Size:         info.Size(),
		LastModified: info.ModTime().Unix(),
	}, nil
}

func (p *localProvider) Close() error {
	p.closed.Store(true)
	return nil
}
