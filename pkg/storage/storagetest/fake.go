package storagetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

var _ storage.Provider = (*Fake)(nil)

type object struct {
	data         []byte
	lastModified int64
}

// Fake fakes storage.Provider as an in-memory object store with the boundary
// behavior of storage.NewLocal. A set <Method>Func alone decides that
// method's result, after key validation and the closed check. The zero value
// is ready to use. Set the exported fields before the first call; the methods
// are then safe for concurrent use.
type Fake struct {
	GetFunc    func(ctx context.Context, key string) (io.ReadCloser, error)
	PutFunc    func(ctx context.Context, key string, data io.Reader) error
	DeleteFunc func(ctx context.Context, key string) error
	ListFunc   func(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error)
	ExistsFunc func(ctx context.Context, key string) (bool, error)
	StatFunc   func(ctx context.Context, key string) (*storage.ObjectMetadata, error)

	// CloseErr is returned by Close. The provider is closed regardless.
	CloseErr error

	mu      sync.Mutex
	objects map[string]object
	closed  bool
}

// check applies what Local applies before touching storage: the closed check,
// then key validation. The caller holds no lock.
func (f *Fake) check(key string) error {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return storage.ErrProviderClosed
	}
	return storage.ValidateKey(key)
}

// normalize mirrors Local, which stores a key at filepath.Clean(key).
func normalize(key string) string { return filepath.Clean(key) }

func (f *Fake) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := f.check(key); err != nil {
		return nil, err
	}
	if f.GetFunc != nil {
		return f.GetFunc(ctx, key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[normalize(key)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(slices.Clone(obj.data))), nil
}

func (f *Fake) Put(ctx context.Context, key string, data io.Reader) error {
	if err := f.check(key); err != nil {
		return err
	}
	if data == nil {
		return errors.New("storagetest: Put called with a nil reader")
	}
	if f.PutFunc != nil {
		return f.PutFunc(ctx, key, data)
	}
	// Read outside the lock: the reader is caller code and may block.
	buf, err := io.ReadAll(data)
	if err != nil {
		return fmt.Errorf("writing data: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = make(map[string]object)
	}
	f.objects[normalize(key)] = object{data: buf, lastModified: time.Now().Unix()}
	return nil
}

func (f *Fake) Delete(ctx context.Context, key string) error {
	if err := f.check(key); err != nil {
		return err
	}
	if f.DeleteFunc != nil {
		return f.DeleteFunc(ctx, key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, normalize(key))
	return nil
}

// List matches prefix as a path, as Local does: an object matches when its
// key equals the cleaned prefix or lies beneath it.
func (f *Fake) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return nil, storage.ErrProviderClosed
	}
	if prefix != "" {
		if err := storage.ValidateKey(prefix); err != nil {
			return nil, err
		}
	}
	if f.ListFunc != nil {
		return f.ListFunc(ctx, prefix)
	}
	dir := ""
	if prefix != "" {
		dir = normalize(prefix)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []storage.ObjectMetadata
	for key, obj := range f.objects {
		if dir == "" || key == dir || strings.HasPrefix(key, dir+"/") {
			out = append(out, storage.ObjectMetadata{Key: key, Size: int64(len(obj.data)), LastModified: obj.lastModified})
		}
	}
	// Local walks directory by directory, so compare segment by segment: plain
	// string order would put "a-b" and "a.txt" before "a/x".
	slices.SortFunc(out, func(a, b storage.ObjectMetadata) int {
		return slices.Compare(strings.Split(a.Key, "/"), strings.Split(b.Key, "/"))
	})
	return out, nil
}

func (f *Fake) Exists(ctx context.Context, key string) (bool, error) {
	if err := f.check(key); err != nil {
		return false, err
	}
	if f.ExistsFunc != nil {
		return f.ExistsFunc(ctx, key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[normalize(key)]
	return ok, nil
}

func (f *Fake) Stat(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	if err := f.check(key); err != nil {
		return nil, err
	}
	if f.StatFunc != nil {
		return f.StatFunc(ctx, key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[normalize(key)]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectMetadata{Key: key, Size: int64(len(obj.data)), LastModified: obj.lastModified}, nil
}

// Close marks the provider closed, as Local does, and returns CloseErr.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return f.CloseErr
}

// Objects returns a deep copy of the stored objects keyed by normalized key.
func (f *Fake) Objects() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string][]byte, len(f.objects))
	for key, obj := range f.objects {
		out[key] = slices.Clone(obj.data)
	}
	return out
}
