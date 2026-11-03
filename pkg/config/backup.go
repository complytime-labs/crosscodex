package config

import (
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// BackupConfig configures `crosscodexd admin backup`. An empty
// Destination.Backend disables backup; every backup command then fails closed.
type BackupConfig struct {
	// DSN is a postgres:// URL for backup_user (LOGIN REPLICATION). WAL-G
	// streams base backups over it and the objects step reads tenant IDs
	// with it. Never logged.
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
	var p problems
	d := b.Destination
	if !validBackupBackends[d.Backend] {
		p.add("backup.destination.backend", "backup.destination.backend %q%s must be one of local, s3, or empty (backup disabled): %w",
			d.Backend, formatSource(tracker, "backup.destination.backend"), ErrInvalidConfig)
	}
	for _, f := range []struct {
		name  string
		value time.Duration
	}{{"postgres", b.MaxAge.Postgres}, {"objects", b.MaxAge.Objects}, {"nats", b.MaxAge.NATS}} {
		if f.value <= 0 {
			p.add("backup.max_age."+f.name, "backup.max_age.%s %s%s must be greater than zero; `admin backup verify` reports the store as stale once its newest backup is older than this: %w",
				f.name, f.value, formatSource(tracker, "backup.max_age."+f.name), ErrInvalidConfig)
		}
	}
	if b.DSN != "" {
		// The value is never echoed: it may hold a password, and url.Parse
		// errors quote their input.
		u, err := url.Parse(b.DSN)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			p.add("backup.dsn", "backup.dsn%s must be a postgres:// URL for backup_user (value not shown): %w",
				formatSource(tracker, "backup.dsn"), ErrInvalidConfig)
		}
	}
	if d.Backend == "" || !validBackupBackends[d.Backend] {
		// Disabled, or already reported: nothing below applies.
		return p.err()
	}
	backendSource := tracker.sourceOf("backup.destination.backend")
	if b.DSN == "" {
		p.addFrom(backendSource, "backup.dsn", "backup.dsn is required when backup.destination.backend is %q%s; set it (or CROSSCODEX_BACKUP_DSN) to a postgres:// URL for backup_user: %w",
			d.Backend, formatSource(tracker, "backup.destination.backend"), ErrMissingRequired)
	}
	switch d.Backend {
	case "local":
		if !filepath.IsAbs(d.Local.Path) {
			p.add("backup.destination.local.path", "backup.destination.local.path %q%s must be an absolute path: %w",
				d.Local.Path, formatSource(tracker, "backup.destination.local.path"), ErrInvalidConfig)
		} else if objects.Backend == "local" && objects.BasePath != "" && pathsOverlap(d.Local.Path, objects.BasePath) {
			// Reported under the backup path, the setting the backup feature
			// adds; storage.objects.base_path is the tenants' data.
			p.add("backup.destination.local.path", "backup.destination.local.path %q%s overlaps storage.objects.base_path %q: the backup repository would share the tenant directory tree. Use a separate directory: %w",
				d.Local.Path, formatSource(tracker, "backup.destination.local.path"), objects.BasePath, ErrInvalidConfig)
		}
	case "s3":
		if d.S3.Bucket == "" {
			p.addFrom(backendSource, "backup.destination.s3.bucket", "backup.destination.s3.bucket is required when backup.destination.backend is s3%s: %w",
				formatSource(tracker, "backup.destination.backend"), ErrMissingRequired)
		}
	}
	return p.err()
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
