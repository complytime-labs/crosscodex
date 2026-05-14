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
)

type localProvider struct {
	root     string // absolute path: {root}/{tenantID}/
	realRoot string // symlink-resolved version of root
	tenantID string
	closed   atomic.Bool
}

// NewLocal creates a Provider backed by the local filesystem.
// All operations are scoped to {root}/{tenantID}/.
func NewLocal(root, tenantID string) (Provider, error) {
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

	return &localProvider{
		root:     tenantRoot,
		realRoot: realRoot,
		tenantID: tenantID,
	}, nil
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

	path, err := p.resolveAndVerify(key)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("opening file: %w", err)
	}
	return f, nil
}

func (p *localProvider) Put(ctx context.Context, key string, data io.Reader) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}

	path, err := p.resolveAndVerify(key)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directories: %w", err)
	}

	// Re-verify after creating directories to close TOCTOU window.
	if _, err := p.resolveAndVerify(key); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
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
		return fmt.Errorf("writing data: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	success = true
	return nil
}

func (p *localProvider) Delete(ctx context.Context, key string) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}

	path, err := p.resolveAndVerify(key)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing file: %w", err)
	}
	return nil
}

func (p *localProvider) List(ctx context.Context, prefix string) ([]ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}

	searchRoot := p.root
	if prefix != "" {
		if err := validateKey(prefix); err != nil {
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
		return nil, fmt.Errorf("walking directory: %w", err)
	}

	return result, nil
}

func (p *localProvider) Exists(ctx context.Context, key string) (bool, error) {
	if p.closed.Load() {
		return false, ErrProviderClosed
	}

	path, err := p.resolveAndVerify(key)
	if err != nil {
		return false, err
	}

	_, err = os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat: %w", err)
	}
	return true, nil
}

func (p *localProvider) Stat(ctx context.Context, key string) (*ObjectMetadata, error) {
	if p.closed.Load() {
		return nil, ErrProviderClosed
	}

	path, err := p.resolveAndVerify(key)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat: %w", err)
	}

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
