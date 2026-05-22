package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func TestLocal_ProviderSuite(t *testing.T) {
	t.Parallel()
	providerTestSuite(t, func(t *testing.T) Provider {
		t.Helper()
		p, _ := newTestLocal(t)
		return p
	})
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

func TestLocal_InvalidKeys_AllOps(t *testing.T) {
	t.Parallel()
	p, _ := newTestLocal(t)
	ctx := context.Background()

	// The shared suite tests Put and Get for invalid keys.
	// This test covers the remaining operations: Delete, Stat, Exists.
	keys := []string{"", "/etc/passwd", "../escape", "docs/../../escape", "docs/\x00evil"}
	for _, key := range keys {
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
