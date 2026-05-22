package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"
)

// providerTestSuite runs the standard Provider interface tests against any backend.
// Tests cover CRUD operations, listing, existence checks, stat, close behavior,
// and key validation. Backend-specific tests (symlink escape, permissions, tenant
// prefix isolation, etc.) remain in their respective test files.
func providerTestSuite(t *testing.T, newProvider func(t *testing.T) Provider) {
	t.Helper()

	t.Run("PutThenGet", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("PutOverwrites", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("GetMissing", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, err := p.Get(context.Background(), "nonexistent.json")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteExisting", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("DeleteMissing", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		err := p.Delete(context.Background(), "nonexistent.json")
		if err != nil {
			t.Errorf("Delete(missing) error = %v, want nil", err)
		}
	})

	t.Run("ListWithPrefix", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		ctx := context.Background()

		for _, f := range []string{"docs/a.json", "docs/b.json", "other/c.json"} {
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
			t.Fatalf("List returned %d items, want %d: %v", len(keys), len(want), keys)
		}
		for i, k := range keys {
			if k != want[i] {
				t.Errorf("List[%d] = %q, want %q", i, k, want[i])
			}
		}
	})

	t.Run("ListEmptyPrefix", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		ctx := context.Background()

		if err := p.Put(ctx, "a.json", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(a.json) error: %v", err)
		}
		if err := p.Put(ctx, "docs/b.json", bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(docs/b.json) error: %v", err)
		}

		list, err := p.List(ctx, "")
		if err != nil {
			t.Fatalf("List() error: %v", err)
		}
		if len(list) != 2 {
			t.Errorf("List(\"\") returned %d items, want 2", len(list))
		}
	})

	t.Run("ExistsFound", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("ExistsNotFound", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		ok, err := p.Exists(context.Background(), "nonexistent.json")
		if err != nil {
			t.Fatalf("Exists() error: %v", err)
		}
		if ok {
			t.Errorf("Exists(missing) = true, want false")
		}
	})

	t.Run("StatExisting", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("StatMissing", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		_, err := p.Stat(context.Background(), "missing.json")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
		}
	})

	t.Run("OperationsAfterClose", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
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
	})

	t.Run("InvalidKeys", func(t *testing.T) {
		t.Parallel()
		p := newProvider(t)
		ctx := context.Background()

		keys := []string{"", "/etc/passwd", "../escape", "docs/../../escape", "docs/\x00evil"}
		for _, key := range keys {
			if err := p.Put(ctx, key, bytes.NewReader([]byte("x"))); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q) error = %v, want ErrInvalidKey", key, err)
			}
			if _, err := p.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q) error = %v, want ErrInvalidKey", key, err)
			}
		}
	})
}
