//go:build !integration

package storage_test

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
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestStorageBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Storage System BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// S3 provider interface compliance and tenant prefix isolation are tested
// via the mock S3 client defined below, injected through ExportNewS3WithClient
// from export_test.go.

// ---------------------------------------------------------------------------
// Shared provider behavior specs (ports providerTestSuite)
// ---------------------------------------------------------------------------

func providerBehavior(newProvider func() storage.Provider) func() {
	return func() {
		var p storage.Provider

		BeforeEach(func() {
			p = newProvider()
		})

		AfterEach(func() {
			// Attempt close; ignore errors from already-closed providers
			_ = p.Close()
		})

		Context("CRUD operations", func() {
			It("stores and retrieves objects (Put then Get)", func() {
				ctx := context.Background()
				want := []byte("hello world")

				By("storing an object")
				err := p.Put(ctx, "docs/file.json", bytes.NewReader(want))
				Expect(err).NotTo(HaveOccurred())

				By("retrieving the same object")
				rc, err := p.Get(ctx, "docs/file.json")
				Expect(err).NotTo(HaveOccurred())
				defer rc.Close()

				got, err := io.ReadAll(rc)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(want))
			})

			It("overwrites existing objects on repeated Put", func() {
				ctx := context.Background()

				By("writing version 1")
				Expect(p.Put(ctx, "file.json", bytes.NewReader([]byte("v1")))).To(Succeed())

				By("overwriting with version 2")
				Expect(p.Put(ctx, "file.json", bytes.NewReader([]byte("v2")))).To(Succeed())

				By("reading back the latest version")
				rc, err := p.Get(ctx, "file.json")
				Expect(err).NotTo(HaveOccurred())
				defer rc.Close()
				got, _ := io.ReadAll(rc)
				Expect(string(got)).To(Equal("v2"))
			})

			It("returns ErrNotFound for missing objects", func() {
				_, err := p.Get(context.Background(), "nonexistent.json")
				Expect(err).To(MatchError(storage.ErrNotFound))
			})

			It("deletes existing objects", func() {
				ctx := context.Background()

				By("creating an object")
				Expect(p.Put(ctx, "file.json", bytes.NewReader([]byte("data")))).To(Succeed())

				By("deleting it")
				Expect(p.Delete(ctx, "file.json")).To(Succeed())

				By("confirming it no longer exists")
				_, err := p.Get(ctx, "file.json")
				Expect(err).To(MatchError(storage.ErrNotFound))
			})

			It("treats Delete of missing objects as idempotent (no error)", func() {
				err := p.Delete(context.Background(), "nonexistent.json")
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("listing objects", func() {
			It("lists objects matching a prefix", func() {
				ctx := context.Background()

				By("creating objects in different prefixes")
				for _, f := range []string{"docs/a.json", "docs/b.json", "other/c.json"} {
					Expect(p.Put(ctx, f, bytes.NewReader([]byte("x")))).To(Succeed())
				}

				By("listing only the docs/ prefix")
				list, err := p.List(ctx, "docs/")
				Expect(err).NotTo(HaveOccurred())

				var keys []string
				for _, m := range list {
					keys = append(keys, m.Key)
				}
				sort.Strings(keys)

				Expect(keys).To(Equal([]string{"docs/a.json", "docs/b.json"}))
			})

			It("lists all objects with empty prefix", func() {
				ctx := context.Background()

				By("creating objects")
				Expect(p.Put(ctx, "a.json", bytes.NewReader([]byte("x")))).To(Succeed())
				Expect(p.Put(ctx, "docs/b.json", bytes.NewReader([]byte("x")))).To(Succeed())

				By("listing with empty prefix")
				list, err := p.List(ctx, "")
				Expect(err).NotTo(HaveOccurred())
				Expect(list).To(HaveLen(2))
			})
		})

		Context("existence checks", func() {
			It("reports true for existing objects", func() {
				ctx := context.Background()
				Expect(p.Put(ctx, "file.json", bytes.NewReader([]byte("data")))).To(Succeed())

				ok, err := p.Exists(ctx, "file.json")
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue())
			})

			It("reports false for missing objects", func() {
				ok, err := p.Exists(context.Background(), "nonexistent.json")
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeFalse())
			})
		})

		Context("object metadata (Stat)", func() {
			It("returns correct metadata for existing objects", func() {
				ctx := context.Background()
				data := []byte("stat test data")

				Expect(p.Put(ctx, "stat.json", bytes.NewReader(data))).To(Succeed())

				meta, err := p.Stat(ctx, "stat.json")
				Expect(err).NotTo(HaveOccurred())
				Expect(meta.Key).To(Equal("stat.json"))
				Expect(meta.Size).To(Equal(int64(len(data))))
				Expect(meta.LastModified).NotTo(BeZero())
			})

			It("returns ErrNotFound for missing objects", func() {
				_, err := p.Stat(context.Background(), "missing.json")
				Expect(err).To(MatchError(storage.ErrNotFound))
			})
		})

		Context("lifecycle (Close)", func() {
			It("rejects all operations after Close", func() {
				ctx := context.Background()

				By("closing the provider")
				Expect(p.Close()).To(Succeed())

				By("verifying Put is rejected")
				err := p.Put(ctx, "f.json", bytes.NewReader([]byte("x")))
				Expect(err).To(MatchError(storage.ErrProviderClosed))

				By("verifying Get is rejected")
				_, err = p.Get(ctx, "f.json")
				Expect(err).To(MatchError(storage.ErrProviderClosed))

				By("verifying Delete is rejected")
				err = p.Delete(ctx, "f.json")
				Expect(err).To(MatchError(storage.ErrProviderClosed))

				By("verifying List is rejected")
				_, err = p.List(ctx, "")
				Expect(err).To(MatchError(storage.ErrProviderClosed))

				By("verifying Stat is rejected")
				_, err = p.Stat(ctx, "f.json")
				Expect(err).To(MatchError(storage.ErrProviderClosed))

				By("verifying Exists is rejected")
				_, err = p.Exists(ctx, "f.json")
				Expect(err).To(MatchError(storage.ErrProviderClosed))
			})
		})

		Context("key validation", func() {
			It("rejects invalid keys for Put and Get", func() {
				ctx := context.Background()
				keys := []string{"", "/etc/passwd", "../escape", "docs/../../escape", "docs/\x00evil"}

				for _, key := range keys {
					By(fmt.Sprintf("rejecting Put with key %q", key))
					err := p.Put(ctx, key, bytes.NewReader([]byte("x")))
					Expect(err).To(MatchError(storage.ErrInvalidKey))

					By(fmt.Sprintf("rejecting Get with key %q", key))
					_, err = p.Get(ctx, key)
					Expect(err).To(MatchError(storage.ErrInvalidKey))
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Mock S3 client (ported from s3_test.go)
// ---------------------------------------------------------------------------

type bddTestAPIError struct {
	code    string
	message string
}

func (e *bddTestAPIError) Error() string                 { return e.message }
func (e *bddTestAPIError) ErrorCode() string             { return e.code }
func (e *bddTestAPIError) ErrorMessage() string          { return e.message }
func (e *bddTestAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

type bddMockObject struct {
	data         []byte
	lastModified time.Time
	contentType  string
}

type bddMockS3 struct {
	mu      sync.Mutex
	objects map[string]bddMockObject
}

func newBDDMockS3() *bddMockS3 {
	return &bddMockS3{objects: make(map[string]bddMockObject)}
}

func (m *bddMockS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	m.objects[aws.ToString(input.Key)] = bddMockObject{
		data:         data,
		lastModified: time.Now(),
		contentType:  aws.ToString(input.ContentType),
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *bddMockS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &bddTestAPIError{code: "NoSuchKey", message: "key not found"}
	}
	size := int64(len(obj.data))
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.data)),
		ContentLength: &size,
	}, nil
}

func (m *bddMockS3) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (m *bddMockS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
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

func (m *bddMockS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, &bddTestAPIError{code: "NotFound", message: "key not found"}
	}
	size := int64(len(obj.data))
	modTime := obj.lastModified
	return &s3.HeadObjectOutput{
		ContentLength: &size,
		ContentType:   &obj.contentType,
		LastModified:  &modTime,
	}, nil
}

// ---------------------------------------------------------------------------
// BDD Suite
// ---------------------------------------------------------------------------

var _ = Describe("Storage System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Storage System BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Storage System BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// =================================================================

	Describe("Content-Addressable Storage Behaviors", func() {
		Context("when computing content hashes for attestation integrity", func() {
			It("produces deterministic SHA-256 hashes to guarantee content verification", func() {
				By("hashing a known input to a known digest")
				got := storage.ContentHash([]byte("hello world"))
				Expect(got).To(Equal("b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9")) // DevSkim: ignore DS173237 - SHA-256 test vector, not a credential

				By("confirming determinism across repeated calls")
				first := storage.ContentHash([]byte("determinism test"))
				second := storage.ContentHash([]byte("determinism test"))
				Expect(first).To(Equal(second))
			})

			It("handles edge-case inputs without errors", func() {
				By("hashing empty input")
				empty := storage.ContentHash([]byte{})
				Expect(empty).To(Equal("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")) // DevSkim: ignore DS173237 - SHA-256 test vector, not a credential

				By("hashing binary data")
				binary := storage.ContentHash([]byte{0x00, 0x01, 0x02, 0xff})
				Expect(binary).To(Equal("3d1f57c984978ef98a18378c8166c1cb8ede02c03eeb6aee7e2f121dfeee3e56")) // DevSkim: ignore DS173237 - SHA-256 test vector, not a credential
			})

			It("derives attestation storage keys from content hash", func() {
				data := []byte("hello world")
				hash := storage.ContentHash(data)
				want := "attestation/" + hash + ".json"

				got := storage.ContentKey(data)
				Expect(got).To(Equal(want))
			})
		})
	})

	Describe("JobAttestationKey", func() {
		It("produces correct path for layout", func() {
			key := storage.JobAttestationKey("job-123", "layout.json")
			Expect(key).To(Equal("jobs/job-123/attestation/layout.json"))
		})

		It("produces correct path for link", func() {
			key := storage.JobAttestationKey("job-123", "ingestion.link.json")
			Expect(key).To(Equal("jobs/job-123/attestation/ingestion.link.json"))
		})

		It("produces correct path for manifest", func() {
			key := storage.JobAttestationKey("job-456", "input_manifest.sha256")
			Expect(key).To(Equal("jobs/job-456/attestation/input_manifest.sha256"))
		})
	})

	Describe("Sentinel Error Behaviors", func() {
		Context("when distinguishing storage failure modes", func() {
			It("defines non-nil sentinel errors for all failure categories", func() {
				Expect(storage.ErrNotFound).NotTo(BeNil())
				Expect(storage.ErrInvalidKey).NotTo(BeNil())
				Expect(storage.ErrProviderClosed).NotTo(BeNil())
			})

			It("keeps sentinel errors distinct to prevent misidentification", func() {
				sentinels := []error{storage.ErrNotFound, storage.ErrInvalidKey, storage.ErrProviderClosed}
				for i := 0; i < len(sentinels); i++ {
					for j := i + 1; j < len(sentinels); j++ {
						Expect(errors.Is(sentinels[i], sentinels[j])).To(BeFalse(),
							"sentinel %d and %d should be distinct", i, j)
					}
				}
			})

			It("allows sentinel matching through error wrapping chains", func() {
				wrapped := fmt.Errorf("get failed: %w", storage.ErrNotFound)
				Expect(errors.Is(wrapped, storage.ErrNotFound)).To(BeTrue())
			})
		})
	})

	Describe("Tenant Isolation in Storage", func() {
		Context("when enforcing multi-tenant data boundaries on local filesystem", func() {
			It("prevents cross-tenant file access via path traversal", func() {
				root := GinkgoT().TempDir()

				By("creating two tenant providers sharing the same root")
				pA, err := storage.NewLocal(root, "tenant-a")
				Expect(err).NotTo(HaveOccurred())
				defer pA.Close()

				_, err = storage.NewLocal(root, "tenant-b")
				Expect(err).NotTo(HaveOccurred())

				By("planting a file in tenant-b's directory")
				secretPath := filepath.Join(root, "tenant-b", "secret.json")
				Expect(os.WriteFile(secretPath, []byte("tenant-b-secret"), 0600)).To(Succeed())

				By("attempting cross-tenant access from tenant-a")
				_, err = pA.Get(context.Background(), "../tenant-b/secret.json")
				Expect(err).To(MatchError(storage.ErrInvalidKey))
			})
		})

		Context("when enforcing tenant prefix isolation on S3", func() {
			It("rejects empty tenant ID on S3 constructor", func() {
				_, err := storage.NewS3("bucket", "")
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			})

			It("rejects empty bucket name", func() {
				_, err := storage.NewS3("", "test-tenant")
				Expect(err).To(MatchError(storage.ErrBucketRequired))
			})
		})

		Context("when enforcing tenant validation on local constructor", func() {
			It("rejects empty tenant ID", func() {
				_, err := storage.NewLocal(GinkgoT().TempDir(), "")
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			})
		})
	})

	Describe("Factory Configuration Behaviors", func() {
		Context("when creating providers from application config", func() {
			It("creates a local provider using XDG_DATA_HOME", func() {
				xdgDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_DATA_HOME", xdgDir)

				p, err := storage.NewFromConfig(config.ObjectStorageConfig{Backend: "local"}, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				expectedDir := filepath.Join(xdgDir, "crosscodex", "objects", "test-tenant")
				info, err := os.Stat(expectedDir)
				Expect(err).NotTo(HaveOccurred())
				Expect(info.IsDir()).To(BeTrue())
			})

			It("falls back to HOME/.local/share when XDG_DATA_HOME is unset", func() {
				homeDir := GinkgoT().TempDir()
				GinkgoT().Setenv("HOME", homeDir)
				GinkgoT().Setenv("XDG_DATA_HOME", "")

				p, err := storage.NewFromConfig(config.ObjectStorageConfig{Backend: "local"}, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				expectedDir := filepath.Join(homeDir, ".local", "share", "crosscodex", "objects", "test-tenant")
				_, err = os.Stat(expectedDir)
				Expect(err).NotTo(HaveOccurred())
			})

			It("prefers explicit BasePath over XDG default", func() {
				xdgDir := GinkgoT().TempDir()
				GinkgoT().Setenv("XDG_DATA_HOME", xdgDir)
				explicitDir := GinkgoT().TempDir()

				p, err := storage.NewFromConfig(config.ObjectStorageConfig{
					Backend:  "local",
					BasePath: explicitDir,
				}, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				By("confirming explicit path was used")
				_, err = os.Stat(filepath.Join(explicitDir, "test-tenant"))
				Expect(err).NotTo(HaveOccurred())

				By("confirming XDG path was NOT created")
				xdgObjects := filepath.Join(xdgDir, "crosscodex", "objects")
				_, err = os.Stat(xdgObjects)
				Expect(os.IsNotExist(err)).To(BeTrue())
			})

			It("creates a local provider with explicit BasePath", func() {
				p, err := storage.NewFromConfig(config.ObjectStorageConfig{
					Backend:  "local",
					BasePath: GinkgoT().TempDir(),
				}, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()
			})

			It("rejects unsupported backends", func() {
				_, err := storage.NewFromConfig(config.ObjectStorageConfig{Backend: "gcs"}, "test-tenant")
				Expect(err).To(HaveOccurred())
			})

			It("rejects empty backend", func() {
				_, err := storage.NewFromConfig(config.ObjectStorageConfig{}, "test-tenant")
				Expect(err).To(HaveOccurred())
			})

			It("rejects empty tenant for local backend", func() {
				_, err := storage.NewFromConfig(config.ObjectStorageConfig{
					Backend:  "local",
					BasePath: GinkgoT().TempDir(),
				}, "")
				Expect(err).To(HaveOccurred())
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// =================================================================

	Describe("Provider Interface Compliance", func() {
		Context("Local filesystem provider", providerBehavior(func() storage.Provider {
			p, err := storage.NewLocal(GinkgoT().TempDir(), "test-tenant")
			Expect(err).NotTo(HaveOccurred())
			return p
		}))

		Context("S3 provider (mock)", providerBehavior(func() storage.Provider {
			mock := newBDDMockS3()
			return storage.ExportNewS3WithClient(mock, "test-bucket", "test-tenant")
		}))
	})

	Describe("Telemetry Integration", func() {
		Context("Local provider", func() {
			Context("when created without telemetry", func() {
				It("has nil telemetry fields", func() {
					p, err := storage.NewLocal(GinkgoT().TempDir(), "test-tenant")
					Expect(err).NotTo(HaveOccurred())
					defer p.Close()

					tf := storage.ExportLocalTelemetryFields(p)
					Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
					Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
					Expect(tf.HasOpCounter).To(BeFalse(), "opCounter should be nil without telemetry")
					Expect(tf.HasOpLatency).To(BeFalse(), "opLatency should be nil without telemetry")
				})
			})

			Context("when created with telemetry", func() {
				It("initializes all telemetry instruments", func() {
					tp := tracenoop.NewTracerProvider()
					tracer := tp.Tracer("storage-test")
					mp := metricnoop.NewMeterProvider()
					meter := mp.Meter("storage-test")

					p, err := storage.NewLocal(GinkgoT().TempDir(), "test-tenant", storage.WithLocalTelemetry(tracer, meter))
					Expect(err).NotTo(HaveOccurred())
					defer p.Close()

					tf := storage.ExportLocalTelemetryFields(p)
					Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
					Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
					Expect(tf.HasOpCounter).To(BeTrue(), "opCounter should be set with telemetry")
					Expect(tf.HasOpLatency).To(BeTrue(), "opLatency should be set with telemetry")
				})
			})
		})

		Context("S3 provider", func() {
			Context("when created without telemetry", func() {
				It("has nil telemetry fields", func() {
					mock := newBDDMockS3()
					p := storage.ExportNewS3WithClient(mock, "test-bucket", "test-tenant")

					tf := storage.ExportS3TelemetryFields(p)
					Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
					Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
					Expect(tf.HasOpCounter).To(BeFalse(), "opCounter should be nil without telemetry")
					Expect(tf.HasOpLatency).To(BeFalse(), "opLatency should be nil without telemetry")
				})
			})
		})

		Context("when local operations produce spans (success path)", func() {
			var (
				tp       *telemetrytest.TestProvider
				provider storage.Provider
			)

			BeforeEach(func() {
				var err error
				tp, err = telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())

				tracer := tp.TracerProvider().Tracer("storage-test")
				meter := tp.MeterProvider().Meter("storage-test")
				tmpDir := GinkgoT().TempDir()
				provider, err = storage.NewLocal(tmpDir, "test-tenant",
					storage.WithLocalTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())
			})

			AfterEach(func() {
				_ = provider.Close()
				Expect(tp.Shutdown(context.Background())).To(Succeed())
			})

			It("emits storage.Put and storage.Get spans with storage.key attribute", func() {
				ctx := context.Background()
				err := provider.Put(ctx, "telemetry-test/file.txt",
					bytes.NewReader([]byte("hello")))
				Expect(err).NotTo(HaveOccurred())

				rc, err := provider.Get(ctx, "telemetry-test/file.txt")
				Expect(err).NotTo(HaveOccurred())
				_ = rc.Close()

				spans := tp.GetSpans()

				putSpan := telemetrytest.FindSpan(spans, "storage.Put")
				Expect(putSpan).NotTo(BeNil(), "expected storage.Put span")
				Expect(putSpan.Status().Code.String()).To(Equal("Ok"))
				val, ok := telemetrytest.SpanAttribute(putSpan, "storage.key")
				Expect(ok).To(BeTrue())
				Expect(val.AsString()).To(Equal("telemetry-test/file.txt"))

				getSpan := telemetrytest.FindSpan(spans, "storage.Get")
				Expect(getSpan).NotTo(BeNil(), "expected storage.Get span")
				Expect(getSpan.Status().Code.String()).To(Equal("Ok"))
			})

			It("emits storage.List span with storage.prefix attribute", func() {
				ctx := context.Background()
				_ = provider.Put(ctx, "list-test/a.txt",
					bytes.NewReader([]byte("a")))

				tp.Reset() // clear Put spans

				_, err := provider.List(ctx, "list-test/")
				Expect(err).NotTo(HaveOccurred())

				spans := tp.GetSpans()
				listSpan := telemetrytest.FindSpan(spans, "storage.List")
				Expect(listSpan).NotTo(BeNil(), "expected storage.List span")
				val, ok := telemetrytest.SpanAttribute(listSpan, "storage.prefix")
				Expect(ok).To(BeTrue())
				Expect(val.AsString()).To(Equal("list-test/"))
			})

			It("emits storage.Exists, storage.Stat, and storage.Delete spans", func() {
				ctx := context.Background()
				_ = provider.Put(ctx, "lifecycle/obj.txt",
					bytes.NewReader([]byte("data")))

				tp.Reset()

				exists, err := provider.Exists(ctx, "lifecycle/obj.txt")
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())

				_, err = provider.Stat(ctx, "lifecycle/obj.txt")
				Expect(err).NotTo(HaveOccurred())

				err = provider.Delete(ctx, "lifecycle/obj.txt")
				Expect(err).NotTo(HaveOccurred())

				spans := tp.GetSpans()
				Expect(telemetrytest.FindSpan(spans, "storage.Exists")).NotTo(BeNil())
				Expect(telemetrytest.FindSpan(spans, "storage.Stat")).NotTo(BeNil())
				Expect(telemetrytest.FindSpan(spans, "storage.Delete")).NotTo(BeNil())
			})

			It("records storage.operations.total and storage.operation.duration_ms", func() {
				ctx := context.Background()
				_ = provider.Put(ctx, "metrics-test/x.txt",
					bytes.NewReader([]byte("x")))

				rm := tp.GetMetrics()

				counter := telemetrytest.FindMetric(rm, "storage.operations.total")
				Expect(counter).NotTo(BeNil())
				val, err := telemetrytest.CounterValue(counter)
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNumerically(">=", 1))

				hist := telemetrytest.FindMetric(rm, "storage.operation.duration_ms")
				Expect(hist).NotTo(BeNil())
				count, err := telemetrytest.HistogramCount(hist)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(BeNumerically(">=", 1))
			})

			It("emits storage.Get span with Error status on missing key", func() {
				ctx := context.Background()

				tp.Reset()

				_, err := provider.Get(ctx, "nonexistent/missing.txt")
				Expect(err).To(HaveOccurred())

				spans := tp.GetSpans()
				getSpan := telemetrytest.FindSpan(spans, "storage.Get")
				Expect(getSpan).NotTo(BeNil(), "expected storage.Get span on error path")
				Expect(getSpan.Status().Code.String()).To(Equal("Error"))
			})
		})
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES
	// =================================================================

	Describe("Local Provider Edge Cases", func() {
		Context("when securing file system access", func() {
			It("creates tenant directory with restricted permissions (0700)", func() {
				root := GinkgoT().TempDir()
				_, err := storage.NewLocal(root, "acme")
				Expect(err).NotTo(HaveOccurred())

				info, err := os.Stat(filepath.Join(root, "acme"))
				Expect(err).NotTo(HaveOccurred())
				Expect(info.IsDir()).To(BeTrue())
				Expect(info.Mode().Perm()).To(Equal(os.FileMode(0700)))
			})

			It("writes files with restricted permissions (0600)", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				Expect(p.Put(context.Background(), "secure.json", bytes.NewReader([]byte("secret")))).To(Succeed())

				info, err := os.Stat(filepath.Join(root, "test-tenant", "secure.json"))
				Expect(err).NotTo(HaveOccurred())
				Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
			})

			It("rejects invalid keys for Delete, Stat, and Exists", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				ctx := context.Background()
				keys := []string{"", "/etc/passwd", "../escape", "docs/../../escape", "docs/\x00evil"}

				for _, key := range keys {
					By(fmt.Sprintf("rejecting Delete with key %q", key))
					Expect(p.Delete(ctx, key)).To(MatchError(storage.ErrInvalidKey))

					By(fmt.Sprintf("rejecting Stat with key %q", key))
					_, err := p.Stat(ctx, key)
					Expect(err).To(MatchError(storage.ErrInvalidKey))

					By(fmt.Sprintf("rejecting Exists with key %q", key))
					_, err = p.Exists(ctx, key)
					Expect(err).To(MatchError(storage.ErrInvalidKey))
				}
			})
		})

		Context("when defending against symlink escape attacks", func() {
			It("blocks Get/Put/Stat/Delete through a symlink pointing outside tenant root", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "victim")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				ctx := context.Background()
				tenantDir := filepath.Join(root, "victim")
				outsideDir := GinkgoT().TempDir()

				By("planting a secret outside the tenant root")
				Expect(os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("stolen"), 0600)).To(Succeed())

				By("creating a symlink inside the tenant dir pointing outside")
				Expect(os.Symlink(outsideDir, filepath.Join(tenantDir, "escape"))).To(Succeed())

				By("verifying Get is blocked")
				_, err = p.Get(ctx, "escape/secret.txt")
				Expect(err).To(MatchError(storage.ErrInvalidKey))

				By("verifying Put is blocked")
				err = p.Put(ctx, "escape/planted.txt", bytes.NewReader([]byte("pwned")))
				Expect(err).To(MatchError(storage.ErrInvalidKey))

				By("verifying Stat is blocked")
				_, err = p.Stat(ctx, "escape/secret.txt")
				Expect(err).To(MatchError(storage.ErrInvalidKey))

				By("verifying Delete is blocked")
				err = p.Delete(ctx, "escape/secret.txt")
				Expect(err).To(MatchError(storage.ErrInvalidKey))
			})

			It("blocks chained symlink escapes (a -> b -> outside)", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "victim")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				outsideDir := GinkgoT().TempDir()
				tenantDir := filepath.Join(root, "victim")

				By("creating chained symlinks: a -> b -> outside")
				bLink := filepath.Join(tenantDir, "b")
				Expect(os.Symlink(outsideDir, bLink)).To(Succeed())
				aLink := filepath.Join(tenantDir, "a")
				Expect(os.Symlink(bLink, aLink)).To(Succeed())

				By("verifying Get through the chain is blocked")
				_, err = p.Get(context.Background(), "a/file.txt")
				Expect(err).To(MatchError(storage.ErrInvalidKey))
			})
		})

		Context("when handling concurrent access", func() {
			It("survives concurrent Puts to the same key without corruption", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

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
					Expect(<-errc).To(Succeed())
				}

				By("reading back the final value")
				rc, err := p.Get(ctx, "shared.json")
				Expect(err).NotTo(HaveOccurred())
				defer rc.Close()
				got, _ := io.ReadAll(rc)
				Expect(string(got)).To(HavePrefix("writer-"))
			})

			It("survives concurrent Puts and Gets without corruption", func() {
				root := GinkgoT().TempDir()
				p, err := storage.NewLocal(root, "test-tenant")
				Expect(err).NotTo(HaveOccurred())
				defer p.Close()

				ctx := context.Background()
				original := []byte("original-content")
				Expect(p.Put(ctx, "target.json", bytes.NewReader(original))).To(Succeed())

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
					Expect(<-errc).To(Succeed())
				}
			})
		})
	})

	Describe("Key Validation Edge Cases", func() {
		// These test validateKey behavior indirectly through Put/Get on a real provider
		// since validateKey is unexported.
		var p storage.Provider

		BeforeEach(func() {
			var err error
			p, err = storage.NewLocal(GinkgoT().TempDir(), "test-tenant")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			_ = p.Close()
		})

		DescribeTable("rejects invalid keys",
			func(key string) {
				ctx := context.Background()
				err := p.Put(ctx, key, bytes.NewReader([]byte("x")))
				Expect(err).To(MatchError(storage.ErrInvalidKey))
				_, err = p.Get(ctx, key)
				Expect(err).To(MatchError(storage.ErrInvalidKey))
			},
			Entry("empty key", ""),
			Entry("absolute path", "/etc/passwd"),
			Entry("simple traversal", "../other-tenant/secret"),
			Entry("mid-path traversal", "docs/../../other/secret"),
			Entry("trailing traversal", "docs/.."),
			Entry("dot only", "."),
			Entry("double dot only", ".."),
			Entry("null byte", "docs/\x00evil"),
			Entry("backslash traversal", "docs\\..\\secret"),
		)

		DescribeTable("accepts valid keys",
			func(key string) {
				ctx := context.Background()
				Expect(p.Put(ctx, key, bytes.NewReader([]byte("data")))).To(Succeed())
				rc, err := p.Get(ctx, key)
				Expect(err).NotTo(HaveOccurred())
				defer rc.Close()
				got, _ := io.ReadAll(rc)
				Expect(string(got)).To(Equal("data"))
			},
			Entry("valid flat", "file.json"),
			Entry("valid nested", "documents/sub/file.json"),
			Entry("valid dotfile", "documents/.hidden"),
			Entry("valid deep path", "a/b/c/d/e.json"),
		)
	})

	Describe("Tenant ID Validation Edge Cases", func() {
		// These test validateTenantID behavior indirectly through constructors
		// since validateTenantID is unexported.

		DescribeTable("rejects invalid tenant IDs in NewLocal",
			func(tenantID string) {
				_, err := storage.NewLocal(GinkgoT().TempDir(), tenantID)
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			},
			Entry("empty tenant", ""),
			Entry("leading digit", "550e8400-e29b-41d4-a716-446655440000"),
		)

		It("accepts valid tenant IDs in NewLocal", func() {
			_, err := storage.NewLocal(GinkgoT().TempDir(), "acme-corp")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("S3 Provider Edge Cases", func() {
		Context("when validating S3 constructor inputs", func() {
			It("rejects empty tenant ID", func() {
				_, err := storage.NewS3("bucket", "")
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			})

			It("rejects empty bucket name", func() {
				_, err := storage.NewS3("", "test-tenant")
				Expect(err).To(MatchError(storage.ErrBucketRequired))
			})
		})

		Context("when enforcing tenant prefix isolation on S3 (mock)", func() {
			It("isolates data between tenants sharing the same bucket", func() {
				mock := newBDDMockS3()
				pA := storage.ExportNewS3WithClient(mock, "bucket", "tenant-a")
				pB := storage.ExportNewS3WithClient(mock, "bucket", "tenant-b")
				ctx := context.Background()

				By("storing data under tenant-a")
				Expect(pA.Put(ctx, "shared.json", bytes.NewReader([]byte("from-a")))).To(Succeed())

				By("storing data under tenant-b with the same key")
				Expect(pB.Put(ctx, "shared.json", bytes.NewReader([]byte("from-b")))).To(Succeed())

				By("reading tenant-a's data")
				rcA, err := pA.Get(ctx, "shared.json")
				Expect(err).NotTo(HaveOccurred())
				defer rcA.Close()
				gotA, _ := io.ReadAll(rcA)
				Expect(string(gotA)).To(Equal("from-a"))

				By("reading tenant-b's data")
				rcB, err := pB.Get(ctx, "shared.json")
				Expect(err).NotTo(HaveOccurred())
				defer rcB.Close()
				gotB, _ := io.ReadAll(rcB)
				Expect(string(gotB)).To(Equal("from-b"))
			})
		})
	})

	// =================================================================
	// LEVEL 4: INTERNAL FUNCTION SPECIFICATIONS
	// (ported from old-style tests via export_test.go)
	// =================================================================

	Describe("internal: validateKey", func() {
		DescribeTable("rejects invalid keys",
			func(key string) {
				err := storage.ExportValidateKey(key)
				Expect(err).To(MatchError(storage.ErrInvalidKey))
			},
			Entry("empty key", ""),
			Entry("absolute path", "/etc/passwd"),
			Entry("simple traversal", "../other-tenant/secret"),
			Entry("mid-path traversal", "docs/../../other/secret"),
			Entry("trailing traversal", "docs/.."),
			Entry("dot only", "."),
			Entry("double dot only", ".."),
			Entry("null byte", "docs/\x00evil"),
			Entry("backslash traversal", "docs\\..\\secret"),
		)

		DescribeTable("accepts valid keys",
			func(key string) {
				err := storage.ExportValidateKey(key)
				Expect(err).NotTo(HaveOccurred())
			},
			Entry("valid flat", "file.json"),
			Entry("valid nested", "documents/sub/file.json"),
			Entry("valid dotfile", "documents/.hidden"),
			Entry("valid deep path", "a/b/c/d/e.json"),
		)
	})

	Describe("internal: validateTenantID", func() {
		DescribeTable("rejects invalid tenant IDs",
			func(tenantID string) {
				err := storage.ExportValidateTenantID(tenantID)
				Expect(err).To(MatchError(tenant.ErrInvalidTenant))
			},
			Entry("empty tenant", ""),
			Entry("leading digit", "550e8400-e29b-41d4-a716-446655440000"),
		)

		It("accepts valid tenant IDs", func() {
			err := storage.ExportValidateTenantID("acme-corp")
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("internal: xdgDataHome", func() {
		It("uses XDG_DATA_HOME when set", func() {
			GinkgoT().Setenv("XDG_DATA_HOME", "/custom/data")
			got := storage.ExportXdgDataHome()
			Expect(got).To(Equal("/custom/data"))
		})

		It("falls back to HOME/.local/share when XDG_DATA_HOME is unset", func() {
			GinkgoT().Setenv("XDG_DATA_HOME", "")
			GinkgoT().Setenv("HOME", "/home/testuser")
			got := storage.ExportXdgDataHome()
			Expect(got).To(Equal("/home/testuser/.local/share"))
		})
	})
})
