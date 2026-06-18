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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestStorageIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Storage Integration Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var (
	testEndpoint  string
	testAccessKey string
	testSecretKey string
	testBucket    string
)

var _ = BeforeSuite(func() {
	testEndpoint = os.Getenv("TEST_S3_ENDPOINT")
	if testEndpoint == "" {
		Skip("TEST_S3_ENDPOINT not set — run: task test:integration:storage")
	}
	testAccessKey = os.Getenv("TEST_S3_ACCESS_KEY")
	testSecretKey = os.Getenv("TEST_S3_SECRET_KEY")
	testBucket = os.Getenv("TEST_S3_BUCKET")
	if testBucket == "" {
		testBucket = "crosscodex-test"
	}

	Expect(ensureBucket(testEndpoint, testAccessKey, testSecretKey, testBucket)).To(Succeed())
})

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
func newIntegrationProvider(tenantID string) storage.Provider {
	p, err := storage.NewS3(testBucket, tenantID,
		storage.WithEndpoint(testEndpoint),
		storage.WithRegion("us-east-1"),
		storage.WithCredentials(testAccessKey, testSecretKey),
	)
	Expect(err).NotTo(HaveOccurred())
	return p
}

// newIntegrationProviderWithTelemetry creates a Provider with OpenTelemetry
// tracing and metrics enabled.
func newIntegrationProviderWithTelemetry(tenantID string, tracer trace.Tracer, meter metric.Meter) storage.Provider {
	p, err := storage.NewS3(testBucket, tenantID,
		storage.WithEndpoint(testEndpoint),
		storage.WithRegion("us-east-1"),
		storage.WithCredentials(testAccessKey, testSecretKey),
		storage.WithS3Telemetry(tracer, meter),
	)
	Expect(err).NotTo(HaveOccurred())
	return p
}

// cleanupKeys deletes the given keys from the provider, ignoring errors.
func cleanupKeys(p storage.Provider, keys ...string) {
	ctx := context.Background()
	for _, k := range keys {
		_ = p.Delete(ctx, k)
	}
}

// getAndCompare reads the given key and asserts its content matches want.
func getAndCompare(p storage.Provider, key string, want []byte) {
	rc, err := p.Get(context.Background(), key)
	Expect(err).NotTo(HaveOccurred(), "Get(%s) error", key)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	Expect(err).NotTo(HaveOccurred(), "ReadAll error")
	Expect(got).To(Equal(want), "Get(%s) content mismatch", key)
}

// listAndCompareKeys lists objects with the given prefix, sorts the keys, and
// asserts they match want exactly.
func listAndCompareKeys(p storage.Provider, prefix string, want []string) {
	list, err := p.List(context.Background(), prefix)
	Expect(err).NotTo(HaveOccurred(), "List(%s) error", prefix)

	var keys []string
	for _, m := range list {
		keys = append(keys, m.Key)
	}
	sort.Strings(keys)

	Expect(keys).To(HaveLen(len(want)), "List returned unexpected item count: %v", keys)
	for i, k := range keys {
		Expect(k).To(Equal(want[i]), "List[%d] mismatch", i)
	}
}

var _ = Describe("S3 Integration", func() {
	Context("Put then Get", func() {
		It("should store and retrieve an object", func() {
			p := newIntegrationProvider("integ-put-get")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			key := "put-get/file.json"
			want := []byte(`{"hello":"s3"}`)
			DeferCleanup(func() { cleanupKeys(p, key) })

			Expect(p.Put(ctx, key, bytes.NewReader(want))).To(Succeed())
			getAndCompare(p, key, want)
		})
	})

	Context("Put overwrite", func() {
		It("should overwrite an existing object", func() {
			p := newIntegrationProvider("integ-overwrite")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			key := "overwrite/file.json"
			DeferCleanup(func() { cleanupKeys(p, key) })

			Expect(p.Put(ctx, key, bytes.NewReader([]byte("v1")))).To(Succeed())
			Expect(p.Put(ctx, key, bytes.NewReader([]byte("v2")))).To(Succeed())
			getAndCompare(p, key, []byte("v2"))
		})
	})

	Context("Get missing", func() {
		It("should return ErrNotFound for a nonexistent key", func() {
			p := newIntegrationProvider("integ-get-missing")
			DeferCleanup(func() { _ = p.Close() })

			_, err := p.Get(context.Background(), "no-such-key/missing.json")
			Expect(errors.Is(err, storage.ErrNotFound)).To(BeTrue(),
				"expected ErrNotFound, got: %v", err)
		})
	})

	Context("Delete existing", func() {
		It("should delete an object and return ErrNotFound on subsequent Get", func() {
			p := newIntegrationProvider("integ-del-exist")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			key := "del-exist/file.json"
			DeferCleanup(func() { cleanupKeys(p, key) })

			Expect(p.Put(ctx, key, bytes.NewReader([]byte("data")))).To(Succeed())
			Expect(p.Delete(ctx, key)).To(Succeed())

			_, err := p.Get(ctx, key)
			Expect(errors.Is(err, storage.ErrNotFound)).To(BeTrue(),
				"expected ErrNotFound after delete, got: %v", err)
		})
	})

	Context("Delete missing", func() {
		It("should succeed when deleting a nonexistent key", func() {
			p := newIntegrationProvider("integ-del-missing")
			DeferCleanup(func() { _ = p.Close() })

			err := p.Delete(context.Background(), "no-such-key/missing.json")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("List with prefix", func() {
		It("should list only objects matching the prefix", func() {
			p := newIntegrationProvider("integ-list-prefix")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			files := []string{"docs/a.json", "docs/b.json", "other/c.json"}
			DeferCleanup(func() { cleanupKeys(p, files...) })

			for _, f := range files {
				Expect(p.Put(ctx, f, bytes.NewReader([]byte("x")))).To(Succeed())
			}

			listAndCompareKeys(p, "docs/", []string{"docs/a.json", "docs/b.json"})
		})
	})

	Context("List empty", func() {
		It("should return an empty list for a nonexistent prefix", func() {
			p := newIntegrationProvider("integ-list-empty")
			DeferCleanup(func() { _ = p.Close() })

			list, err := p.List(context.Background(), "nonexistent-prefix/")
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(BeEmpty())
		})
	})

	Context("Exists found", func() {
		It("should return true for an existing key", func() {
			p := newIntegrationProvider("integ-exists-found")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			key := "exists-found/file.json"
			DeferCleanup(func() { cleanupKeys(p, key) })

			Expect(p.Put(ctx, key, bytes.NewReader([]byte("data")))).To(Succeed())

			ok, err := p.Exists(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})
	})

	Context("Exists not found", func() {
		It("should return false for a nonexistent key", func() {
			p := newIntegrationProvider("integ-exists-nf")
			DeferCleanup(func() { _ = p.Close() })

			ok, err := p.Exists(context.Background(), "no-such-key/missing.json")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})
	})

	Context("Stat existing", func() {
		It("should return correct metadata for an existing object", func() {
			p := newIntegrationProvider("integ-stat-exist")
			DeferCleanup(func() { _ = p.Close() })

			ctx := context.Background()
			key := "stat-exist/file.json"
			data := []byte("stat test data")
			DeferCleanup(func() { cleanupKeys(p, key) })

			Expect(p.Put(ctx, key, bytes.NewReader(data))).To(Succeed())

			meta, err := p.Stat(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Key).To(Equal(key))
			Expect(meta.Size).To(Equal(int64(len(data))))
			Expect(meta.LastModified).NotTo(BeZero())
		})
	})

	Context("Stat missing", func() {
		It("should return ErrNotFound for a nonexistent key", func() {
			p := newIntegrationProvider("integ-stat-missing")
			DeferCleanup(func() { _ = p.Close() })

			_, err := p.Stat(context.Background(), "no-such-key/missing.json")
			Expect(errors.Is(err, storage.ErrNotFound)).To(BeTrue(),
				"expected ErrNotFound, got: %v", err)
		})
	})

	Context("Tenant isolation", func() {
		It("should prevent tenants from seeing each other's objects", func() {
			pA := newIntegrationProvider("integ-iso-alpha")
			DeferCleanup(func() { _ = pA.Close() })
			pB := newIntegrationProvider("integ-iso-beta")
			DeferCleanup(func() { _ = pB.Close() })

			ctx := context.Background()
			key := "isolation/shared.json"
			DeferCleanup(func() {
				cleanupKeys(pA, key)
				cleanupKeys(pB, key)
			})

			Expect(pA.Put(ctx, key, bytes.NewReader([]byte("from-alpha")))).To(Succeed())
			Expect(pB.Put(ctx, key, bytes.NewReader([]byte("from-beta")))).To(Succeed())

			getAndCompare(pA, key, []byte("from-alpha"))
			getAndCompare(pB, key, []byte("from-beta"))

			// Alpha listing does not show beta's objects.
			listAndCompareKeys(pA, "isolation/", []string{key})
			// Beta listing does not show alpha's objects.
			listAndCompareKeys(pB, "isolation/", []string{key})
		})
	})

	Context("Telemetry", func() {
		It("should emit spans and metrics for all CRUD operations", func() {
			ctx := context.Background()

			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(ctx) })

			tracer := tp.TracerProvider().Tracer("storage-integration-test")
			meter := tp.MeterProvider().Meter("storage-integration-test")

			p := newIntegrationProviderWithTelemetry("integ-telemetry", tracer, meter)
			DeferCleanup(func() { _ = p.Close() })

			key := "telemetry-crud.json"
			data := []byte(`{"telemetry":"test"}`)
			DeferCleanup(func() { cleanupKeys(p, key) })

			// Put
			Expect(p.Put(ctx, key, bytes.NewReader(data))).To(Succeed())

			// Get
			getAndCompare(p, key, data)

			// List
			listAndCompareKeys(p, "telemetry-", []string{key})

			// Exists
			ok, err := p.Exists(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())

			// Stat
			meta, err := p.Stat(ctx, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Key).To(Equal(key))

			// Delete
			Expect(p.Delete(ctx, key)).To(Succeed())

			// Assert spans
			spans := tp.GetSpans()
			wantSpans := []string{
				"storage.Put",
				"storage.Get",
				"storage.List",
				"storage.Exists",
				"storage.Stat",
				"storage.Delete",
			}
			for _, name := range wantSpans {
				Expect(telemetrytest.FindSpan(spans, name)).NotTo(BeNil(),
					"missing span %q", name)
			}

			// Assert storage.key attribute on Put span
			putSpan := telemetrytest.FindSpan(spans, "storage.Put")
			Expect(putSpan).NotTo(BeNil())
			val, found := telemetrytest.SpanAttribute(putSpan, "storage.key")
			Expect(found).To(BeTrue(), "Put span missing storage.key attribute")
			Expect(val.AsString()).To(Equal(key))

			// Assert metrics
			rm := tp.GetMetrics()
			opMetric := telemetrytest.FindMetric(rm, "storage.operations.total")
			Expect(opMetric).NotTo(BeNil(), "missing metric storage.operations.total")

			count, err := telemetrytest.CounterValue(opMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(BeNumerically(">=", 6))
		})
	})
})
