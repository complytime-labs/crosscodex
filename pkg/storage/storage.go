package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

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
