package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Namespace is the tenant slot the repository occupies in its storage
// backend, so all backup data lives under <destination>/backup/. WAL-G
// writes its own data beside it under <destination>/postgres/.
const Namespace = "backup"

const (
	inProgressName = "IN_PROGRESS"
	manifestName   = "manifest.json"
)

// Repository is the backup destination. It inherits pkg/storage's atomic
// local writes and key/path-escape checks.
type Repository struct {
	store storage.Provider
}

// NewRepository wraps an already-open provider (tests pass a local one).
func NewRepository(store storage.Provider) *Repository {
	return &Repository{store: store}
}

// OpenRepository opens the configured destination. An empty backend means
// backup is disabled, and this fails closed.
func OpenRepository(dest config.BackupDestinationConfig) (*Repository, error) {
	var (
		store storage.Provider
		err   error
	)
	switch dest.Backend {
	case "local":
		store, err = storage.NewLocal(dest.Local.Path, Namespace)
	case "s3":
		var opts []storage.S3Option
		if dest.S3.Region != "" {
			opts = append(opts, storage.WithRegion(dest.S3.Region))
		}
		if dest.S3.Endpoint != "" {
			opts = append(opts, storage.WithEndpoint(dest.S3.Endpoint))
		}
		if dest.S3.StorageClass != "" {
			opts = append(opts, storage.WithStorageClass(dest.S3.StorageClass))
		}
		store, err = storage.NewS3(dest.S3.Bucket, Namespace, opts...)
	default:
		return nil, fmt.Errorf("backup is disabled: backup.destination.backend is %q. Set it to local or s3 and set backup.dsn (see deploy/README.md, Backups), then retry", dest.Backend)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s backup destination: %w", dest.Backend, err)
	}
	return &Repository{store: store}, nil
}

// Close releases the underlying provider.
func (r *Repository) Close() error { return r.store.Close() }

func pointKey(id, name string) string              { return "points/" + id + "/" + name }
func blobKey(sum string) string                    { return "objects/blobs/" + sum }
func streamFileKey(id, stream, name string) string { return "nats/" + id + "/" + stream + "/" + name }

// Begin marks point id as in progress.
func (r *Repository) Begin(ctx context.Context, id string, started time.Time) error {
	if err := ValidatePointID(id); err != nil {
		return err
	}
	if err := r.store.Put(ctx, pointKey(id, inProgressName), strings.NewReader(started.UTC().Format(time.RFC3339Nano))); err != nil {
		return fmt.Errorf("mark point %s in progress: %w", id, err)
	}
	return nil
}

// WriteManifest commits a point: once manifest.json exists the point is complete.
func (r *Repository) WriteManifest(ctx context.Context, m *Manifest) error {
	var buf bytes.Buffer
	if err := Encode(&buf, m); err != nil {
		return err
	}
	if err := r.store.Put(ctx, pointKey(m.ID, manifestName), bytes.NewReader(buf.Bytes())); err != nil {
		return fmt.Errorf("write manifest for point %s: %w", m.ID, err)
	}
	return nil
}

// ClearInProgress removes the IN_PROGRESS marker of a committed point.
func (r *Repository) ClearInProgress(ctx context.Context, id string) error {
	if err := ValidatePointID(id); err != nil {
		return err
	}
	if err := r.store.Delete(ctx, pointKey(id, inProgressName)); err != nil {
		return fmt.Errorf("remove IN_PROGRESS marker of point %s: %w", id, err)
	}
	return nil
}

// PointList holds point IDs in ascending (time) order.
type PointList struct {
	Complete   []string // manifest.json present
	Incomplete []string // IN_PROGRESS only: the run failed or is still going
}

// Points lists every point. Files other than the two markers are ignored
// (e.g. a local backend's temp files); a malformed point directory is an
// error, because it means the repository was modified outside crosscodexd.
func (r *Repository) Points(ctx context.Context) (PointList, error) {
	objs, err := r.store.List(ctx, "points/")
	if err != nil {
		return PointList{}, fmt.Errorf("list backup points: %w", err)
	}
	type markers struct{ manifest, inProgress bool }
	seen := map[string]*markers{}
	for _, o := range objs {
		parts := strings.Split(o.Key, "/")
		if len(parts) != 3 || parts[0] != "points" {
			continue
		}
		id, name := parts[1], parts[2]
		if name != manifestName && name != inProgressName {
			continue
		}
		if err := ValidatePointID(id); err != nil {
			return PointList{}, fmt.Errorf("backup repository holds unexpected entry %q under points/: %w. Only crosscodexd writes there; move it out of the repository, then retry", o.Key, ErrInvalidPointID)
		}
		mk := seen[id]
		if mk == nil {
			mk = &markers{}
			seen[id] = mk
		}
		mk.manifest = mk.manifest || name == manifestName
		mk.inProgress = mk.inProgress || name == inProgressName
	}
	var pl PointList
	for id, mk := range seen {
		if mk.manifest {
			pl.Complete = append(pl.Complete, id)
		} else {
			pl.Incomplete = append(pl.Incomplete, id)
		}
	}
	slices.Sort(pl.Complete)
	slices.Sort(pl.Incomplete)
	return pl, nil
}

// LoadManifest reads and strictly decodes point id's manifest.
func (r *Repository) LoadManifest(ctx context.Context, id string) (*Manifest, error) {
	if err := ValidatePointID(id); err != nil {
		return nil, err
	}
	rc, err := r.store.Get(ctx, pointKey(id, manifestName))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fmt.Errorf("backup point %s has no manifest (missing or incomplete); choose a complete point from `crosscodexd admin backup list`: %w", id, err)
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest of point %s: %w", id, err)
	}
	m, err := Decode(rc)
	err = errors.Join(err, rc.Close())
	if err != nil {
		return nil, fmt.Errorf("point %s: %w", id, err)
	}
	if m.ID != id {
		return nil, invalidf("point %s holds the manifest of point %s; the repository was modified outside crosscodexd. Restore from another point", id, m.ID)
	}
	return m, nil
}

// HasBlob reports whether blob sum is stored.
func (r *Repository) HasBlob(ctx context.Context, sum string) (bool, error) {
	if !sha256Pattern.MatchString(sum) {
		return false, fmt.Errorf("%w: blob name %q is not a sha256 hex digest", ErrInvalidManifest, sum)
	}
	return r.store.Exists(ctx, blobKey(sum))
}

// PutBlob stores data as blob sum. data must be seekable for S3 (pass a spooled *os.File).
func (r *Repository) PutBlob(ctx context.Context, sum string, data io.Reader) error {
	if !sha256Pattern.MatchString(sum) {
		return fmt.Errorf("%w: blob name %q is not a sha256 hex digest", ErrInvalidManifest, sum)
	}
	return r.store.Put(ctx, blobKey(sum), data)
}

// OpenBlob opens blob sum; storage.ErrNotFound when it's missing.
func (r *Repository) OpenBlob(ctx context.Context, sum string) (io.ReadCloser, error) {
	if !sha256Pattern.MatchString(sum) {
		return nil, fmt.Errorf("%w: blob name %q is not a sha256 hex digest", ErrInvalidManifest, sum)
	}
	return r.store.Get(ctx, blobKey(sum))
}

// validateStreamFileRef checks the point ID, audit stream name, and
// snapshot file name used to key a stream file, so a malformed caller can't
// turn PutStreamFile/OpenStreamFile into a path escape outside nats/<id>/<stream>/.
func validateStreamFileRef(id, stream, name string) error {
	if err := ValidatePointID(id); err != nil {
		return err
	}
	known := natsbus.AuditStreamNames()
	if !slices.Contains(known, stream) {
		return fmt.Errorf("%w: %q is not an audit stream (want one of %v)", ErrInvalidManifest, stream, known)
	}
	if name != snapshotMetaFile && name != snapshotDataFile && name != snapshotLegacyDataFile {
		return fmt.Errorf("%w: %q is not a snapshot file (want %s, %s, or %s)", ErrInvalidManifest, name, snapshotMetaFile, snapshotDataFile, snapshotLegacyDataFile)
	}
	return nil
}

// PutStreamFile stores one snapshot file of stream for point id.
func (r *Repository) PutStreamFile(ctx context.Context, id, stream, name string, data io.Reader) error {
	if err := validateStreamFileRef(id, stream, name); err != nil {
		return err
	}
	return r.store.Put(ctx, streamFileKey(id, stream, name), data)
}

// OpenStreamFile opens one snapshot file; storage.ErrNotFound when it's missing.
func (r *Repository) OpenStreamFile(ctx context.Context, id, stream, name string) (io.ReadCloser, error) {
	if err := validateStreamFileRef(id, stream, name); err != nil {
		return nil, err
	}
	return r.store.Get(ctx, streamFileKey(id, stream, name))
}
