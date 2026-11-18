package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// ErrArchiveDisabled is returned by NewArchiveFromConfig when cfg.Backend is
// empty, indicating that the archive tier is intentionally disabled.
var ErrArchiveDisabled = errors.New("archive backend disabled")

func xdgDataHome() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share")
}

type configOptions struct {
	tracer trace.Tracer
	meter  metric.Meter
}

// ConfigOption configures NewFromConfig.
type ConfigOption func(*configOptions)

// WithTelemetry instruments the provider NewFromConfig returns, whichever
// backend it selects (WithLocalTelemetry or WithS3Telemetry).
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) ConfigOption {
	return func(o *configOptions) {
		o.tracer = tracer
		o.meter = meter
	}
}

// NewFromConfig creates a Provider based on the given configuration.
// It dispatches to NewLocal or NewS3 based on cfg.Backend.
// For local backends, if BasePath is empty it defaults to
// $XDG_DATA_HOME/crosscodex/objects.
func NewFromConfig(cfg config.ObjectStorageConfig, tenantID string, opts ...ConfigOption) (Provider, error) {
	var o configOptions
	for _, opt := range opts {
		opt(&o)
	}
	instrumented := o.tracer != nil && o.meter != nil
	switch ProviderType(cfg.Backend) {
	case ProviderTypeLocal:
		var localOpts []LocalOption
		if instrumented {
			localOpts = append(localOpts, WithLocalTelemetry(o.tracer, o.meter))
		}
		basePath := cfg.BasePath
		if basePath == "" {
			basePath = filepath.Join(xdgDataHome(), "crosscodex", "objects")
		}
		return NewLocal(basePath, tenantID, localOpts...)
	case ProviderTypeS3:
		var s3Opts []S3Option
		if cfg.Region != "" {
			s3Opts = append(s3Opts, WithRegion(cfg.Region))
		}
		if cfg.Endpoint != "" {
			s3Opts = append(s3Opts, WithEndpoint(cfg.Endpoint))
		}
		if instrumented {
			s3Opts = append(s3Opts, WithS3Telemetry(o.tracer, o.meter))
		}
		return NewS3(cfg.Bucket, tenantID, s3Opts...)
	default:
		return nil, fmt.Errorf("unsupported storage backend: %q", cfg.Backend)
	}
}

// NewArchiveFromConfig creates a Provider for the archive (cold-tier) backend
// described by cfg. A disabled archive (empty Backend) returns ErrArchiveDisabled
// so callers can distinguish "no archive configured" from a real error.
func NewArchiveFromConfig(cfg config.ArchiveConfig, tenantID string) (Provider, error) {
	switch cfg.Backend {
	case "":
		return nil, ErrArchiveDisabled
	case "local":
		return NewLocal(cfg.Local.Path, tenantID)
	case "s3":
		return NewS3(cfg.S3.Bucket, tenantID, WithStorageClass(cfg.S3.StorageClass))
	default:
		return nil, fmt.Errorf("unsupported archive backend: %q", cfg.Backend)
	}
}
