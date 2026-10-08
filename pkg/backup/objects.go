package backup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// TenantLister returns every tenant ID. SQLTenantLister reads them as
// backup_user through its dedicated RLS policy (migration 006).
type TenantLister interface {
	TenantIDs(ctx context.Context) ([]string, error)
}

// ObjectStoreOpener opens one tenant's object store. Production binds
// storage.NewFromConfig to cfg.Storage.Objects, so capture and restore go
// through the same tenant-prefix and path-escape checks as the daemon.
type ObjectStoreOpener func(tenantID string) (storage.Provider, error)

// snapshotObjects copies every object of every tenant into the blob
// repository (skipping blobs already present) and returns the per-tenant
// entries plus the total object bytes.
func snapshotObjects(ctx context.Context, repo *Repository, tenants []string, open ObjectStoreOpener, tmpDir string) (map[string][]ObjectEntry, int64, error) {
	out := make(map[string][]ObjectEntry, len(tenants))
	var total int64
	for _, tenantID := range tenants {
		entries, n, err := snapshotTenantObjects(ctx, repo, tenantID, open, tmpDir)
		if err != nil {
			return nil, 0, err
		}
		out[tenantID] = entries
		total += n
	}
	return out, total, nil
}

func snapshotTenantObjects(ctx context.Context, repo *Repository, tenantID string, open ObjectStoreOpener, tmpDir string) (entries []ObjectEntry, total int64, err error) {
	store, err := open(tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("open object store for tenant %q: %w", tenantID, err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close object store for tenant %q: %w", tenantID, cerr))
		}
	}()
	objs, err := store.List(ctx, "")
	if err != nil {
		return nil, 0, fmt.Errorf("list objects for tenant %q: %w", tenantID, err)
	}
	slices.SortFunc(objs, func(a, b storage.ObjectMetadata) int { return strings.Compare(a.Key, b.Key) })
	entries = make([]ObjectEntry, 0, len(objs))
	for _, o := range objs {
		e, found, err := snapshotObject(ctx, repo, store, tenantID, o.Key, tmpDir)
		if err != nil {
			return nil, 0, err
		}
		if found {
			entries = append(entries, e)
			total += e.Size
		}
	}
	return entries, total, nil
}

// snapshotObject returns found=false when the object vanished between
// List and Get (deleted, or on the local backend a temp file renamed into
// place): it no longer exists at capture time, so there is nothing to keep.
func snapshotObject(ctx context.Context, repo *Repository, store storage.Provider, tenantID, key, tmpDir string) (entry ObjectEntry, found bool, err error) {
	rc, err := store.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return ObjectEntry{}, false, nil
	}
	if err != nil {
		return ObjectEntry{}, false, fmt.Errorf("read tenant %q object %q: %w", tenantID, key, err)
	}
	f, sum, size, err := spool(tmpDir, rc)
	cerr := rc.Close()
	if err != nil {
		return ObjectEntry{}, false, fmt.Errorf("read tenant %q object %q: %w", tenantID, key, errors.Join(err, cerr))
	}
	defer func() { err = errors.Join(err, closeAndRemove(f)) }()
	if cerr != nil {
		return ObjectEntry{}, false, fmt.Errorf("read tenant %q object %q: %w", tenantID, key, cerr)
	}
	exists, err := repo.HasBlob(ctx, sum)
	if err != nil {
		return ObjectEntry{}, false, fmt.Errorf("check blob %s: %w", sum, err)
	}
	if !exists {
		if err := repo.PutBlob(ctx, sum, f); err != nil {
			return ObjectEntry{}, false, fmt.Errorf("upload blob %s (tenant %q object %q): %w", sum, tenantID, key, err)
		}
	}
	return ObjectEntry{Key: key, SHA256: sum, Size: size}, true, nil
}

// preflightObjects refuses unless every target tenant store is empty.
// Restore runs it before writing anything, so a refusal changes nothing.
func preflightObjects(ctx context.Context, tenants map[string][]ObjectEntry, open ObjectStoreOpener) error {
	for _, tenantID := range slices.Sorted(maps.Keys(tenants)) {
		store, err := open(tenantID)
		if err != nil {
			return fmt.Errorf("open object store for tenant %q: %w", tenantID, err)
		}
		objs, err := store.List(ctx, "")
		cerr := store.Close()
		if err != nil {
			return fmt.Errorf("list objects for tenant %q: %w", tenantID, errors.Join(err, cerr))
		}
		if cerr != nil {
			return fmt.Errorf("close object store for tenant %q: %w", tenantID, cerr)
		}
		if len(objs) > 0 {
			return fmt.Errorf("cannot restore objects for tenant %q: its object store already holds %d object(s), e.g. %q. Restore never overwrites; empty that tenant's object prefix (or point storage.objects at an empty backend), then retry",
				tenantID, len(objs), objs[0].Key)
		}
	}
	return nil
}

// restoreObjects writes every entry from its blob, checking each blob's
// hash before the write.
func restoreObjects(ctx context.Context, repo *Repository, tenants map[string][]ObjectEntry, open ObjectStoreOpener, tmpDir string) error {
	for _, tenantID := range slices.Sorted(maps.Keys(tenants)) {
		if err := restoreTenantObjects(ctx, repo, tenantID, tenants[tenantID], open, tmpDir); err != nil {
			return err
		}
	}
	return nil
}

func restoreTenantObjects(ctx context.Context, repo *Repository, tenantID string, entries []ObjectEntry, open ObjectStoreOpener, tmpDir string) (err error) {
	store, err := open(tenantID)
	if err != nil {
		return fmt.Errorf("open object store for tenant %q: %w", tenantID, err)
	}
	defer func() {
		if cerr := store.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close object store for tenant %q: %w", tenantID, cerr))
		}
	}()
	for _, e := range entries {
		if err := restoreObject(ctx, repo, store, tenantID, e, tmpDir); err != nil {
			return err
		}
	}
	return nil
}

func restoreObject(ctx context.Context, repo *Repository, store storage.Provider, tenantID string, e ObjectEntry, tmpDir string) (err error) {
	rc, err := repo.OpenBlob(ctx, e.SHA256)
	if err != nil {
		return fmt.Errorf("read blob %s for tenant %q object %q: %w", e.SHA256, tenantID, e.Key, err)
	}
	f, sum, size, err := spool(tmpDir, rc)
	cerr := rc.Close()
	if err != nil {
		return fmt.Errorf("read blob %s for tenant %q object %q: %w", e.SHA256, tenantID, e.Key, errors.Join(err, cerr))
	}
	defer func() { err = errors.Join(err, closeAndRemove(f)) }()
	if cerr != nil {
		return fmt.Errorf("read blob %s for tenant %q object %q: %w", e.SHA256, tenantID, e.Key, cerr)
	}
	if sum != e.SHA256 || size != e.Size {
		return fmt.Errorf("blob %s for tenant %q object %q failed its checksum (content is %d bytes with sha256 %s); the repository is damaged. Run `crosscodexd admin backup verify` and restore from another point",
			e.SHA256, tenantID, e.Key, size, sum)
	}
	if err := store.Put(ctx, e.Key, f); err != nil {
		return fmt.Errorf("write tenant %q object %q: %w", tenantID, e.Key, err)
	}
	return nil
}

// verifyObjects re-hashes every distinct blob a point references. Missing
// or mismatched blobs are issues; I/O failures are errors.
func verifyObjects(ctx context.Context, repo *Repository, tenants map[string][]ObjectEntry) ([]string, error) {
	type ref struct {
		tenantID, key string
		size          int64
	}
	blobs := map[string]ref{}
	for _, tenantID := range slices.Sorted(maps.Keys(tenants)) {
		for _, e := range tenants[tenantID] {
			if _, ok := blobs[e.SHA256]; !ok {
				blobs[e.SHA256] = ref{tenantID: tenantID, key: e.Key, size: e.Size}
			}
		}
	}
	var issues []string
	for _, sum := range slices.Sorted(maps.Keys(blobs)) {
		r := blobs[sum]
		rc, err := repo.OpenBlob(ctx, sum)
		if errors.Is(err, storage.ErrNotFound) {
			issues = append(issues, fmt.Sprintf("blob %s is missing (tenant %q object %q)", sum, r.tenantID, r.key))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read blob %s: %w", sum, err)
		}
		got, size, err := hashReader(rc)
		err = errors.Join(err, rc.Close())
		if err != nil {
			return nil, fmt.Errorf("read blob %s: %w", sum, err)
		}
		if got != sum || size != r.size {
			issues = append(issues, fmt.Sprintf("blob %s sha256 mismatch: content is %d bytes hashing to %s (tenant %q object %q)", sum, size, got, r.tenantID, r.key))
		}
	}
	return issues, nil
}
