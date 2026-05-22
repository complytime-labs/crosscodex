//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

var (
	testEndpoint  string
	testAccessKey string
	testSecretKey string
	testBucket    string
)

func TestMain(m *testing.M) {
	testEndpoint = os.Getenv("TEST_S3_ENDPOINT")
	if testEndpoint == "" {
		fmt.Fprintln(os.Stderr, "TEST_S3_ENDPOINT not set — run: task test:integration:storage")
		os.Exit(1)
	}
	testAccessKey = os.Getenv("TEST_S3_ACCESS_KEY")
	testSecretKey = os.Getenv("TEST_S3_SECRET_KEY")
	testBucket = os.Getenv("TEST_S3_BUCKET")
	if testBucket == "" {
		testBucket = "crosscodex-test"
	}

	if err := ensureBucket(testEndpoint, testAccessKey, testSecretKey, testBucket); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create test bucket: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// ensureBucket creates the test bucket if it does not already exist.
func ensureBucket(endpoint, accessKey, secretKey, bucket string) error {
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		// Bucket may already exist; check for BucketAlreadyOwnedByYou or
		// BucketAlreadyExists which are non-fatal.
		errMsg := err.Error()
		if strings.Contains(errMsg, "BucketAlreadyOwnedByYou") ||
			strings.Contains(errMsg, "BucketAlreadyExists") {
			return nil
		}
		return fmt.Errorf("creating bucket %q: %w", bucket, err)
	}
	return nil
}

// newIntegrationProvider creates a Provider connected to the RustFS container.
func newIntegrationProvider(t *testing.T, tenantID string) storage.Provider {
	t.Helper()
	p, err := storage.NewS3(testBucket, tenantID,
		storage.WithEndpoint(testEndpoint),
		storage.WithRegion("us-east-1"),
		storage.WithCredentials(testAccessKey, testSecretKey),
	)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// cleanupKeys deletes the given keys from the provider, ignoring errors.
func cleanupKeys(t *testing.T, p storage.Provider, keys ...string) {
	t.Helper()
	ctx := context.Background()
	for _, k := range keys {
		_ = p.Delete(ctx, k)
	}
}

// getAndCompare reads the given key and asserts its content matches want.
func getAndCompare(t *testing.T, p storage.Provider, key string, want []byte) {
	t.Helper()
	rc, err := p.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s) error: %v", key, err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get(%s) = %q, want %q", key, got, want)
	}
}

// listAndCompareKeys lists objects with the given prefix, sorts the keys, and
// asserts they match want exactly.
func listAndCompareKeys(t *testing.T, p storage.Provider, prefix string, want []string) {
	t.Helper()
	list, err := p.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%s) error: %v", prefix, err)
	}

	var keys []string
	for _, m := range list {
		keys = append(keys, m.Key)
	}
	sort.Strings(keys)

	if len(keys) != len(want) {
		t.Fatalf("List returned %d items, want %d: %v", len(keys), len(want), keys)
	}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("List[%d] = %q, want %q", i, k, want[i])
		}
	}
}

func TestIntegrationS3_PutThenGet(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-put-get")
	ctx := context.Background()
	key := "put-get/file.json"
	want := []byte(`{"hello":"s3"}`)

	t.Cleanup(func() { cleanupKeys(t, p, key) })

	if err := p.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	getAndCompare(t, p, key, want)
}

func TestIntegrationS3_PutOverwrite(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-overwrite")
	ctx := context.Background()
	key := "overwrite/file.json"

	t.Cleanup(func() { cleanupKeys(t, p, key) })

	if err := p.Put(ctx, key, bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatalf("Put(v1) error: %v", err)
	}
	if err := p.Put(ctx, key, bytes.NewReader([]byte("v2"))); err != nil {
		t.Fatalf("Put(v2) error: %v", err)
	}

	getAndCompare(t, p, key, []byte("v2"))
}

func TestIntegrationS3_GetMissing(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-get-missing")
	_, err := p.Get(context.Background(), "no-such-key/missing.json")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestIntegrationS3_DeleteExisting(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-del-exist")
	ctx := context.Background()
	key := "del-exist/file.json"

	t.Cleanup(func() { cleanupKeys(t, p, key) })

	if err := p.Put(ctx, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("Put() error: %v", err)
	}
	if err := p.Delete(ctx, key); err != nil {
		t.Errorf("Delete() error: %v", err)
	}

	_, err := p.Get(ctx, key)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(deleted) error = %v, want ErrNotFound", err)
	}
}

func TestIntegrationS3_DeleteMissing(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-del-missing")
	err := p.Delete(context.Background(), "no-such-key/missing.json")
	if err != nil {
		t.Errorf("Delete(missing) error = %v, want nil", err)
	}
}

func TestIntegrationS3_ListWithPrefix(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-list-prefix")
	ctx := context.Background()
	files := []string{"docs/a.json", "docs/b.json", "other/c.json"}

	t.Cleanup(func() { cleanupKeys(t, p, files...) })

	for _, f := range files {
		if err := p.Put(ctx, f, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(%s) error: %v", f, err)
		}
	}

	listAndCompareKeys(t, p, "docs/", []string{"docs/a.json", "docs/b.json"})
}

func TestIntegrationS3_ListEmpty(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-list-empty")

	list, err := p.List(context.Background(), "nonexistent-prefix/")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List(empty) returned %d items, want 0", len(list))
	}
}

func TestIntegrationS3_ExistsFound(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-exists-found")
	ctx := context.Background()
	key := "exists-found/file.json"

	t.Cleanup(func() { cleanupKeys(t, p, key) })

	if err := p.Put(ctx, key, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	ok, err := p.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if !ok {
		t.Errorf("Exists() = false, want true")
	}
}

func TestIntegrationS3_ExistsNotFound(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-exists-nf")

	ok, err := p.Exists(context.Background(), "no-such-key/missing.json")
	if err != nil {
		t.Fatalf("Exists() error: %v", err)
	}
	if ok {
		t.Errorf("Exists(missing) = true, want false")
	}
}

func TestIntegrationS3_StatExisting(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-stat-exist")
	ctx := context.Background()
	key := "stat-exist/file.json"
	data := []byte("stat test data")

	t.Cleanup(func() { cleanupKeys(t, p, key) })

	if err := p.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put() error: %v", err)
	}

	meta, err := p.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if meta.Key != key {
		t.Errorf("Stat().Key = %q, want %q", meta.Key, key)
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("Stat().Size = %d, want %d", meta.Size, len(data))
	}
	if meta.LastModified == 0 {
		t.Errorf("Stat().LastModified = 0, want nonzero")
	}
}

func TestIntegrationS3_StatMissing(t *testing.T) {
	t.Parallel()
	p := newIntegrationProvider(t, "integ-stat-missing")

	_, err := p.Stat(context.Background(), "no-such-key/missing.json")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
}

func TestIntegrationS3_TenantIsolation(t *testing.T) {
	t.Parallel()
	pA := newIntegrationProvider(t, "integ-iso-alpha")
	pB := newIntegrationProvider(t, "integ-iso-beta")
	ctx := context.Background()
	key := "isolation/shared.json"

	t.Cleanup(func() {
		cleanupKeys(t, pA, key)
		cleanupKeys(t, pB, key)
	})

	if err := pA.Put(ctx, key, bytes.NewReader([]byte("from-alpha"))); err != nil {
		t.Fatalf("Put(alpha) error: %v", err)
	}
	if err := pB.Put(ctx, key, bytes.NewReader([]byte("from-beta"))); err != nil {
		t.Fatalf("Put(beta) error: %v", err)
	}

	getAndCompare(t, pA, key, []byte("from-alpha"))
	getAndCompare(t, pB, key, []byte("from-beta"))

	// Alpha listing does not show beta's objects.
	listAndCompareKeys(t, pA, "isolation/", []string{key})

	// Beta listing does not show alpha's objects.
	listAndCompareKeys(t, pB, "isolation/", []string{key})
}
