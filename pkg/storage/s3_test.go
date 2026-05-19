package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

type testAPIError struct {
	code    string
	message string
}

func (e *testAPIError) Error() string                 { return e.message }
func (e *testAPIError) ErrorCode() string             { return e.code }
func (e *testAPIError) ErrorMessage() string          { return e.message }
func (e *testAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

type mockObject struct {
	data         []byte
	lastModified time.Time
	contentType  string
}

type mockS3 struct {
	mu      sync.Mutex
	objects map[string]mockObject
}

func newMockS3() *mockS3 {
	return &mockS3{objects: make(map[string]mockObject)}
}

func (m *mockS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	m.objects[aws.ToString(input.Key)] = mockObject{
		data:         data,
		lastModified: time.Now(),
		contentType:  aws.ToString(input.ContentType),
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *mockS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &testAPIError{code: "NoSuchKey", message: "key not found"}
	}
	size := int64(len(obj.data))
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.data)),
		ContentLength: &size,
	}, nil
}

func (m *mockS3) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *mockS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := aws.ToString(input.Prefix)
	var contents []s3types.Object
	for k, obj := range m.objects {
		if strings.HasPrefix(k, prefix) {
			key := k
			size := int64(len(obj.data))
			modTime := obj.lastModified
			contents = append(contents, s3types.Object{
				Key:          &key,
				Size:         &size,
				LastModified: &modTime,
			})
		}
	}
	isTruncated := false
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: &isTruncated,
	}, nil
}

func (m *mockS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &testAPIError{code: "NotFound", message: "key not found"}
	}
	size := int64(len(obj.data))
	modTime := obj.lastModified
	return &s3.HeadObjectOutput{
		ContentLength: &size,
		ContentType:   &obj.contentType,
		LastModified:  &modTime,
	}, nil
}

func newTestS3(t *testing.T) (Provider, *mockS3) {
	t.Helper()
	mock := newMockS3()
	p := newS3WithClient(mock, "test-bucket", "test-tenant")
	return p, mock
}

func TestNewS3_EmptyTenant(t *testing.T) {
	t.Parallel()
	_, err := NewS3("bucket", "")
	if !errors.Is(err, tenant.ErrInvalidTenant) {
		t.Errorf("NewS3(bucket, \"\") error = %v, want tenant.ErrInvalidTenant", err)
	}
}

func TestNewS3_EmptyBucket(t *testing.T) {
	t.Parallel()
	_, err := NewS3("", "tenant")
	if !errors.Is(err, ErrBucketRequired) {
		t.Errorf("NewS3(\"\", tenant) error = %v, want ErrBucketRequired", err)
	}
}

func TestS3_PutThenGet(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	ctx := context.Background()
	want := []byte("hello s3")

	if err := p.Put(ctx, "docs/file.json", bytes.NewReader(want)); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	rc, err := p.Get(ctx, "docs/file.json")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, want) {
		t.Errorf("Get() = %q, want %q", got, want)
	}
}

func TestS3_PutOverwrites(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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

func TestS3_GetMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	_, err := p.Get(context.Background(), "nonexistent.json")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestS3_DeleteExisting(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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

func TestS3_DeleteMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	err := p.Delete(context.Background(), "nonexistent.json")
	if err != nil {
		t.Errorf("Delete(missing) error = %v, want nil", err)
	}
}

func TestS3_ListWithPrefix(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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
}

func TestS3_ListEmptyPrefix(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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
}

func TestS3_ExistsFound(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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

func TestS3_ExistsNotFound(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	ok, err := p.Exists(context.Background(), "nonexistent.json")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if ok {
		t.Errorf("Exists(missing) = true, want false")
	}
}

func TestS3_StatExisting(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	ctx := context.Background()
	data := []byte("stat test")

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

func TestS3_StatMissing(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
	_, err := p.Stat(context.Background(), "missing.json")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
}

func TestS3_OperationsAfterClose(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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

func TestS3_InvalidKeys(t *testing.T) {
	t.Parallel()
	p, _ := newTestS3(t)
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
}

func TestS3_TenantPrefixIsolation(t *testing.T) {
	t.Parallel()
	mock := newMockS3()
	pA := newS3WithClient(mock, "bucket", "tenant-a")
	pB := newS3WithClient(mock, "bucket", "tenant-b")
	ctx := context.Background()

	if err := pA.Put(ctx, "shared.json", bytes.NewReader([]byte("from-a"))); err != nil {
		t.Fatalf("Put(tenant-a) error: %v", err)
	}
	if err := pB.Put(ctx, "shared.json", bytes.NewReader([]byte("from-b"))); err != nil {
		t.Fatalf("Put(tenant-b) error: %v", err)
	}

	rcA, err := pA.Get(ctx, "shared.json")
	if err != nil {
		t.Fatalf("Get(tenant-a) error: %v", err)
	}
	defer func() { _ = rcA.Close() }()
	gotA, _ := io.ReadAll(rcA)

	rcB, err := pB.Get(ctx, "shared.json")
	if err != nil {
		t.Fatalf("Get(tenant-b) error: %v", err)
	}
	defer func() { _ = rcB.Close() }()
	gotB, _ := io.ReadAll(rcB)

	if string(gotA) != "from-a" {
		t.Errorf("tenant-a Get() = %q, want %q", gotA, "from-a")
	}
	if string(gotB) != "from-b" {
		t.Errorf("tenant-b Get() = %q, want %q", gotB, "from-b")
	}
}
