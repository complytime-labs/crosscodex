package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// BackupConfig configures `crosscodexd admin backup`. An empty
// Destination.Backend disables backup; every backup command then fails closed.
type BackupConfig struct {
	// DSN is a postgres:// URL for backup_user (LOGIN REPLICATION, migration
	// 006). WAL-G streams base backups over it and the objects step reads
	// tenant IDs with it. Never logged.
	DSN         string                  `yaml:"dsn" json:"dsn"`
	Destination BackupDestinationConfig `yaml:"destination" json:"destination"`
	MaxAge      BackupMaxAgeConfig      `yaml:"max_age" json:"max_age"`
}

// BackupDestinationConfig selects where backup points and WAL-G data live.
// Backup data goes under <destination>/backup/, WAL-G data under
// <destination>/postgres/.
type BackupDestinationConfig struct {
	Backend string            `yaml:"backend" json:"backend"` // "local", "s3", or "" (disabled)
	Local   BackupLocalConfig `yaml:"local" json:"local"`
	S3      BackupS3Config    `yaml:"s3" json:"s3"`
}

// BackupLocalConfig is a filesystem destination.
type BackupLocalConfig struct {
	Path string `yaml:"path" json:"path"` // absolute; must not overlap storage.objects.base_path
}

// BackupS3Config is an S3 destination. Use a dedicated bucket: pkg/storage
// has no key-prefix option. Credentials come from the AWS credential chain.
type BackupS3Config struct {
	Bucket       string `yaml:"bucket" json:"bucket"`
	Region       string `yaml:"region" json:"region"`
	Endpoint     string `yaml:"endpoint" json:"endpoint"` // S3-compatible stores; enables path-style addressing
	StorageClass string `yaml:"storage_class" json:"storage_class"`
}

// BackupMaxAgeConfig is the age after which `admin backup verify` reports a
// store's newest backup as stale.
type BackupMaxAgeConfig struct {
	Postgres time.Duration `yaml:"postgres" json:"postgres"`
	Objects  time.Duration `yaml:"objects" json:"objects"`
	NATS     time.Duration `yaml:"nats" json:"nats"`
}

var validBackupBackends = map[string]bool{"": true, "local": true, "s3": true}

// validateBackup checks the backup section. objects is consulted so a local
// destination cannot share a directory tree with the tenant object store:
// the repository occupies the "backup" tenant slot of its root.
func validateBackup(b *BackupConfig, objects *ObjectStorageConfig, tracker *sourceTracker) error {
	d := b.Destination
	if !validBackupBackends[d.Backend] {
		return fmt.Errorf("backup.destination.backend %q%s must be one of local, s3, or empty (backup disabled): %w",
			d.Backend, formatSource(tracker, "backup.destination.backend"), ErrInvalidConfig)
	}
	for _, f := range []struct {
		name  string
		value time.Duration
	}{{"postgres", b.MaxAge.Postgres}, {"objects", b.MaxAge.Objects}, {"nats", b.MaxAge.NATS}} {
		if f.value <= 0 {
			return fmt.Errorf("backup.max_age.%s %s%s must be greater than zero; `admin backup verify` reports the store as stale once its newest backup is older than this: %w",
				f.name, f.value, formatSource(tracker, "backup.max_age."+f.name), ErrInvalidConfig)
		}
	}
	if b.DSN != "" {
		// The value is never echoed: it may hold a password, and url.Parse
		// errors quote their input.
		u, err := url.Parse(b.DSN)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			return fmt.Errorf("backup.dsn%s must be a postgres:// URL for backup_user (value not shown): %w",
				formatSource(tracker, "backup.dsn"), ErrInvalidConfig)
		}
	}
	if d.Backend == "" {
		return nil
	}
	if b.DSN == "" {
		return fmt.Errorf("backup.dsn is required when backup.destination.backend is %q; set it (or CROSSCODEX_BACKUP_DSN) to a postgres:// URL for backup_user: %w",
			d.Backend, ErrMissingRequired)
	}
	switch d.Backend {
	case "local":
		if !filepath.IsAbs(d.Local.Path) {
			return fmt.Errorf("backup.destination.local.path %q%s must be an absolute path: %w",
				d.Local.Path, formatSource(tracker, "backup.destination.local.path"), ErrInvalidConfig)
		}
		if objects.Backend == "local" && objects.BasePath != "" && pathsOverlap(d.Local.Path, objects.BasePath) {
			return fmt.Errorf("backup.destination.local.path %q overlaps storage.objects.base_path %q: the backup repository would share the tenant directory tree. Use a separate directory: %w",
				d.Local.Path, objects.BasePath, ErrInvalidConfig)
		}
	case "s3":
		if d.S3.Bucket == "" {
			return fmt.Errorf("backup.destination.s3.bucket is required when backup.destination.backend is s3: %w", ErrMissingRequired)
		}
	}
	return nil
}

// pathsOverlap reports whether a and b are the same directory or one contains the other.
func pathsOverlap(a, b string) bool {
	sep := string(filepath.Separator)
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == sep || b == sep {
		return true
	}
	return strings.HasPrefix(a+sep, b+sep) || strings.HasPrefix(b+sep, a+sep)
}
