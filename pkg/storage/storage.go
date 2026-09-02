package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// NewFromConfig creates a Provider based on the given configuration.
// It dispatches to NewLocal or NewS3 based on cfg.Backend.
// For local backends, if BasePath is empty it defaults to
// $XDG_DATA_HOME/crosscodex/objects.
func NewFromConfig(cfg config.ObjectStorageConfig, tenantID string) (Provider, error) {
	switch ProviderType(cfg.Backend) {
	case ProviderTypeLocal:
		basePath := cfg.BasePath
		if basePath == "" {
			basePath = filepath.Join(xdgDataHome(), "crosscodex", "objects")
		}
		return NewLocal(basePath, tenantID)
	case ProviderTypeS3:
		var opts []S3Option
		if cfg.Region != "" {
			opts = append(opts, WithRegion(cfg.Region))
		}
		if cfg.Endpoint != "" {
			opts = append(opts, WithEndpoint(cfg.Endpoint))
		}
		return NewS3(cfg.Bucket, tenantID, opts...)
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
