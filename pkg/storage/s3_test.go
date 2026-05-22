package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
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

func TestS3_ProviderSuite(t *testing.T) {
	t.Parallel()
	providerTestSuite(t, func(t *testing.T) Provider {
		t.Helper()
		p, _ := newTestS3(t)
		return p
	})
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
