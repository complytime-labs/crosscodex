package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func newTestLocal(t *testing.T) (Provider, string) {
	t.Helper()
	root := t.TempDir()
	p, err := NewLocal(root, "test-tenant")
	if err != nil {
		t.Fatalf("NewLocal() error: %v", err)
	}
	return p, root
}

func TestNewLocal_EmptyTenant(t *testing.T) {
	t.Parallel()
	_, err := NewLocal(t.TempDir(), "")
	if !errors.Is(err, tenant.ErrInvalidTenant) {
		t.Errorf("NewLocal(root, \"\") error = %v, want tenant.ErrInvalidTenant", err)
	}
}

func TestNewLocal_CreatesTenantDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, err := NewLocal(root, "acme")
	if err != nil {
		t.Fatalf("NewLocal() error: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "acme"))
	if err != nil {
		t.Fatalf("tenant dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("tenant path is not a directory")
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("tenant dir perms = %o, want 0700", info.Mode().Perm())
	}
}

func TestLocal_PutThenGet(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()
	want := []byte("hello world")

	if err := p.Put(ctx, "docs/file.json", bytes.NewReader(want)); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	rc, err := p.Get(ctx, "docs/file.json")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get() = %q, want %q", got, want)
	}
}

func TestLocal_PutOverwrites(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	if err := p.Put(ctx, "file.json", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatalf("Put(v1) error: %v", err)
	}
	if err := p.Put(ctx, "file.json", bytes.NewReader([]byte("v2"))); err != nil {
		t.Fatalf("Put(v2) error: %v", err)
	}

	rc, err := p.Get(ctx, "file.json")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2" {
		t.Errorf("Get() = %q, want %q", got, "v2")
	}
}

func TestLocal_GetMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	_, err := p.Get(context.Background(), "nonexistent.json")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestLocal_DeleteExisting(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	if err := p.Put(ctx, "file.json", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("Put() error: %v", err)
	}
	if err := p.Delete(ctx, "file.json"); err != nil {
		t.Errorf("Delete() error: %v", err)
	}
	_, err := p.Get(ctx, "file.json")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestLocal_DeleteMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	err := p.Delete(context.Background(), "nonexistent.json")
	if err != nil {
		t.Errorf("Delete(missing) error = %v, want nil", err)
	}
}

func TestLocal_ListWithPrefix(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	files := []string{"docs/a.json", "docs/b.json", "other/c.json"}
	for _, f := range files {
		if err := p.Put(ctx, f, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(%s) error: %v", f, err)
		}
	}

	list, err := p.List(ctx, "docs/")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var keys []string
	for _, m := range list {
		keys = append(keys, m.Key)
	}
	sort.Strings(keys)

	want := []string{"docs/a.json", "docs/b.json"}
	if len(keys) != len(want) {
		t.Fatalf("List(docs/) returned %d items, want %d", len(keys), len(want))
	}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("List(docs/)[%d] = %q, want %q", i, k, want[i])
		}
	}
}

func TestLocal_ListEmptyPrefix(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	files := []string{"a.json", "docs/b.json"}
	for _, f := range files {
		if err := p.Put(ctx, f, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(%s) error: %v", f, err)
		}
	}

	list, err := p.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("List(\"\") returned %d items, want 2", len(list))
	}
}

func TestLocal_ExistsFound(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	if err := p.Put(ctx, "file.json", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	ok, err := p.Exists(ctx, "file.json")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if !ok {
		t.Errorf("Exists() = false, want true")
	}
}

func TestLocal_ExistsNotFound(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ok, err := p.Exists(context.Background(), "nonexistent.json")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if ok {
		t.Errorf("Exists(missing) = true, want false")
	}
}

func TestLocal_StatExisting(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()
	data := []byte("stat test data")

	if err := p.Put(ctx, "stat.json", bytes.NewReader(data)); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	meta, err := p.Stat(ctx, "stat.json")
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if meta.Key != "stat.json" {
		t.Errorf("Stat().Key = %q, want %q", meta.Key, "stat.json")
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("Stat().Size = %d, want %d", meta.Size, len(data))
	}
	if meta.LastModified == 0 {
		t.Errorf("Stat().LastModified = 0, want nonzero")
	}
}

func TestLocal_StatMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	_, err := p.Stat(context.Background(), "missing.json")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
}

func TestLocal_OperationsAfterClose(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	if err := p.Put(ctx, "f.json", bytes.NewReader([]byte("x"))); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("Put after Close: error = %v, want ErrProviderClosed", err)
	}
	if _, err := p.Get(ctx, "f.json"); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("Get after Close: error = %v, want ErrProviderClosed", err)
	}
	if err := p.Delete(ctx, "f.json"); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("Delete after Close: error = %v, want ErrProviderClosed", err)
	}
	if _, err := p.List(ctx, ""); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("List after Close: error = %v, want ErrProviderClosed", err)
	}
	if _, err := p.Stat(ctx, "f.json"); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("Stat after Close: error = %v, want ErrProviderClosed", err)
	}
	if _, err := p.Exists(ctx, "f.json"); !errors.Is(err, ErrProviderClosed) {
		t.Errorf("Exists after Close: error = %v, want ErrProviderClosed", err)
	}
}

func TestLocal_PutFilePermissions(t *testing.T) {
	t.Parallel()
	p, root := newTestLocal(t)
	ctx := context.Background()

	if err := p.Put(ctx, "secure.json", bytes.NewReader([]byte("secret"))); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	info, err := os.Stat(filepath.Join(root, "test-tenant", "secure.json"))
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file perms = %o, want 0600", info.Mode().Perm())
	}
}

func TestLocal_InvalidKeys(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	keys := []string{"", "/etc/passwd", "../escape", "docs/../../escape", "docs/\x00evil"}
	for _, key := range keys {
		if err := p.Put(ctx, key, bytes.NewReader([]byte("x"))); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if _, err := p.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if err := p.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if _, err := p.Stat(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Stat(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if _, err := p.Exists(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Exists(%q) error = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestLocal_SymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p, err := NewLocal(root, "victim")
	if err != nil {
		t.Fatalf("NewLocal() error: %v", err)
	}
	ctx := context.Background()

	tenantDir := filepath.Join(root, "victim")
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("stolen"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	symlinkPath := filepath.Join(tenantDir, "escape")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatalf("Symlink() error: %v", err)
	}

	_, err = p.Get(ctx, "escape/secret.txt")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Get via symlink escape: error = %v, want ErrInvalidKey", err)
	}

	err = p.Put(ctx, "escape/planted.txt", bytes.NewReader([]byte("pwned")))
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Put via symlink escape: error = %v, want ErrInvalidKey", err)
	}

	_, err = p.Stat(ctx, "escape/secret.txt")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Stat via symlink escape: error = %v, want ErrInvalidKey", err)
	}

	err = p.Delete(ctx, "escape/secret.txt")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Delete via symlink escape: error = %v, want ErrInvalidKey", err)
	}
}

func TestLocal_ChainedSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p, err := NewLocal(root, "victim")
	if err != nil {
		t.Fatalf("NewLocal() error: %v", err)
	}

	outsideDir := t.TempDir()
	tenantDir := filepath.Join(root, "victim")

	bLink := filepath.Join(tenantDir, "b")
	if err := os.Symlink(outsideDir, bLink); err != nil {
		t.Fatalf("Symlink(b) error: %v", err)
	}
	aLink := filepath.Join(tenantDir, "a")
	if err := os.Symlink(bLink, aLink); err != nil {
		t.Fatalf("Symlink(a) error: %v", err)
	}

	_, err = p.Get(context.Background(), "a/file.txt")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("Get via chained symlink: error = %v, want ErrInvalidKey", err)
	}
}

func TestLocal_CrossTenantAccess(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	pA, err := NewLocal(root, "tenant-a")
	if err != nil {
		t.Fatalf("NewLocal(tenant-a) error: %v", err)
	}
	_, err = NewLocal(root, "tenant-b")
	if err != nil {
		t.Fatalf("NewLocal(tenant-b) error: %v", err)
	}

	ctx := context.Background()

	secretPath := filepath.Join(root, "tenant-b", "secret.json")
	if err := os.WriteFile(secretPath, []byte("tenant-b-secret"), 0600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	_, err = pA.Get(ctx, "../tenant-b/secret.json")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("cross-tenant Get error = %v, want ErrInvalidKey", err)
	}
}

func TestLocal_ConcurrentPuts(t *testing.T) {
	p, _ := newTestLocal(t)
	ctx := context.Background()
	const goroutines = 20

	errc := make(chan error, goroutines)
	for i := range goroutines {
		go func(n int) {
			data := []byte(fmt.Sprintf("writer-%d", n))
			errc <- p.Put(ctx, "shared.json", bytes.NewReader(data))
		}(i)
	}

	for range goroutines {
		if err := <-errc; err != nil {
			t.Errorf("concurrent Put error: %v", err)
		}
	}

	rc, err := p.Get(ctx, "shared.json")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !strings.HasPrefix(string(got), "writer-") {
		t.Errorf("concurrent writes produced corrupted data: %q", got)
	}
}

func TestLocal_ConcurrentPutAndGet(t *testing.T) {
	p, _ := newTestLocal(t)
	ctx := context.Background()

	original := []byte("original-content")
	if err := p.Put(ctx, "target.json", bytes.NewReader(original)); err != nil {
		t.Fatalf("initial Put() error: %v", err)
	}

	const goroutines = 20
	errc := make(chan error, goroutines*2)

	for i := range goroutines {
		go func(n int) {
			errc <- p.Put(ctx, "target.json", bytes.NewReader([]byte(fmt.Sprintf("update-%d", n))))
		}(i)
		go func() {
			rc, err := p.Get(ctx, "target.json")
			if err != nil {
				errc <- err
				return
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				errc <- err
				return
			}
			s := string(data)
			if s != "original-content" && !strings.HasPrefix(s, "update-") {
				errc <- fmt.Errorf("corrupted read: %q", s)
				return
			}
			errc <- nil
		}()
	}

	for range goroutines * 2 {
		if err := <-errc; err != nil {
			t.Errorf("concurrent Put+Get error: %v", err)
		}
	}
}
