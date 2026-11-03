package pipeline_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// failingPutStorage is the shared storage fake, except that every write
// fails and is counted.
type failingPutStorage struct {
	*storagetest.Fake
	puts atomic.Int32
}

func (f *failingPutStorage) Put(context.Context, string, io.Reader) error {
	f.puts.Add(1)
	return errors.New("object store unavailable")
}

// verifyCounting is a generator counting its Verify calls: each copies the
// envelope to a temporary file, so none means nothing was written.
type verifyCounting struct {
	attestation.Generator
	verifies atomic.Int32
}

func (v *verifyCounting) Verify(ctx context.Context, data []byte) (*attestation.VerifiedLink, error) {
	v.verifies.Add(1)
	return v.Generator.Verify(ctx, data)
}

var _ = Describe("Result-cache checkpoints", Ordered, func() {
	const (
		tenantA  = "tenant-a"
		digest   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		classify = "classify"
	)
	var (
		trustedGen, otherGen attestation.Generator
		otherKeyID           string
	)

	BeforeAll(func() {
		// Key generation is the expensive part: one trusted and one other
		// key pair serve every spec.
		for _, gen := range []*attestation.Generator{&trustedGen, &otherGen} {
			kp, err := attestation.NewEphemeralKeyProvider()
			Expect(err).NotTo(HaveOccurred())
			*gen, err = attestation.NewGenerator(kp)
			Expect(err).NotTo(HaveOccurred())
			if gen == &otherGen {
				otherKeyID, err = kp.KeyID(context.Background())
				Expect(err).NotTo(HaveOccurred())
			}
		}
	})

	var (
		ctx     context.Context
		objects *storagetest.Fake
		logs    *bytes.Buffer
		tp      *telemetrytest.TestProvider
		job     *pipeline.Job
		att     *pipeline.Attestor
		svc     *pipeline.Service
		cp      analysis.CacheCheckpoints
		counter *verifyCounting
	)

	// serviceOver builds the service the checkpoints run in. Its job store
	// and engine are nil, so any call into them fails the spec.
	serviceOver := func(stor storage.Provider) *pipeline.Service {
		return pipeline.New(nil, nil, nil, nil, pipeline.Attestors{Default: att, StorageRetries: 2}, nil, nil, stor,
			config.PipelineConfig{}, nil,
			pipeline.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
			pipeline.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))
	}

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), tenantA)
		Expect(err).NotTo(HaveOccurred())
		objects = &storagetest.Fake{}
		logs = &bytes.Buffer{}
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		job = &pipeline.Job{JobID: "job-1", TenantID: tenantA, Config: []byte(`{"catalog_id":"cat-1"}`)}
		counter = &verifyCounting{Generator: trustedGen}
		att = &pipeline.Attestor{Generator: counter}
		svc = serviceOver(objects)
		cp = svc.CacheCheckpointsFor(job, att, classify, "embedding")
	})

	refOf := func(jobID, analyzerName string) resultcache.AttemptRef {
		return resultcache.AttemptRef{JobID: jobID, Analyzer: analyzerName, Attempt: uuid.NewString()}
	}

	seal := func(c analysis.CacheCheckpoints, ref resultcache.AttemptRef, d string) {
		signed, err := c.Sign(ctx, ref, d)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, c.Store(ctx, ref, signed)).To(Succeed())
	}

	// plant stores at the checkpoint key of (job-1, classify, attempt) a link
	// gen signs with step, materials and products.
	plant := func(gen attestation.Generator, attempt, step string, materials, products []attestation.Artifact) string {
		link, err := gen.CreateLink(ctx, step, materials, products)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		key := pipeline.CheckpointKey(tenantA, "job-1", classify, attempt)
		ExpectWithOffset(1, objects.Put(ctx, key, bytes.NewReader(link.Raw))).To(Succeed())
		return key
	}
	jobMaterial := []attestation.Artifact{{URI: "job:tenant-a/job-1", Digest: "sha256:cfg"}}

	counted := func(op, outcome string) int64 {
		m := telemetrytest.FindMetric(tp.GetMetrics(), "pipeline.result_cache.checkpoints.total")
		if m == nil {
			return 0
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		Expect(ok).To(BeTrue())
		var n int64
		for _, dp := range sum.DataPoints {
			o, _ := dp.Attributes.Value("operation")
			r, _ := dp.Attributes.Value("outcome")
			if o.AsString() == op && r.AsString() == outcome {
				n += dp.Value
			}
		}
		return n
	}

	Describe("Sign and Store", func() {
		It("stores a link, signed by the tenant's key, that binds the job, analyzer, attempt and digest", func() {
			ref := refOf("job-1", classify)
			seal(cp, ref, digest)

			raw, ok := objects.Objects()[pipeline.CheckpointKey(tenantA, "job-1", classify, ref.Attempt)]
			Expect(ok).To(BeTrue())
			verified, err := trustedGen.Verify(ctx, raw)
			Expect(err).NotTo(HaveOccurred())
			Expect(verified.Step).To(Equal(pipeline.CacheCheckpointStep))
			Expect(verified.Products).To(ConsistOf(attestation.Artifact{
				URI: pipeline.CheckpointURI(tenantA, "job-1", classify, ref.Attempt), Digest: digest}))
			Expect(verified.Materials).To(ContainElement(HaveField("URI", "job:tenant-a/job-1")))
			Expect(counted("sign", "ok")).To(BeEquivalentTo(1))
			Expect(counted("store", "ok")).To(BeEquivalentTo(1))
		})

		It("records operation durations in milliseconds", func() {
			seal(cp, refOf("job-1", classify), digest)
			m := telemetrytest.FindMetric(tp.GetMetrics(), "pipeline.result_cache.checkpoint.duration_ms")
			Expect(m).NotTo(BeNil())
			Expect(m.Unit).To(Equal("ms"))
		})

		It("signs without touching object storage", func() {
			_, err := cp.Sign(ctx, refOf("job-1", classify), digest)
			Expect(err).NotTo(HaveOccurred())
			Expect(objects.Objects()).To(BeEmpty())
		})

		DescribeTable("refuses to sign, writing nothing, a checkpoint for anything but an attempt of this job",
			func(ctxTenant string, ref func() resultcache.AttemptRef, d, want string) {
				c, err := tenant.WithTenant(context.Background(), ctxTenant)
				Expect(err).NotTo(HaveOccurred())
				_, err = cp.Sign(c, ref(), d)
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
				Expect(objects.Objects()).To(BeEmpty())
			},
			Entry("another tenant", "tenant-b", func() resultcache.AttemptRef { return refOf("job-1", classify) }, digest,
				`the context's tenant "tenant-b" is not the job's tenant`),
			Entry("another job", tenantA, func() resultcache.AttemptRef { return refOf("job-2", classify) }, digest,
				`job "job-2" is not job job-1`),
			Entry("an analyzer the job does not run", tenantA, func() resultcache.AttemptRef { return refOf("job-1", "requires") }, digest,
				`analyzer "requires" is not one of the job's`),
			Entry("an attempt that is not a canonical UUID", tenantA, func() resultcache.AttemptRef {
				return resultcache.AttemptRef{JobID: "job-1", Analyzer: classify, Attempt: "../x"}
			}, digest, `attempt "../x" is not a canonical UUID`),
			Entry("a malformed digest", tenantA, func() resultcache.AttemptRef { return refOf("job-1", classify) }, "sha256:zz",
				`digest "sha256:zz" is not sha256:<64 hex>`),
		)

		It("refuses to store an envelope that is not the checkpoint of the attempt it is stored for, writing nothing", func() {
			signed, err := cp.Sign(ctx, refOf("job-1", classify), digest)
			Expect(err).NotTo(HaveOccurred())
			err = cp.Store(ctx, refOf("job-1", classify), signed)
			Expect(err).To(MatchError(ContainSubstring("refusing to store the cache checkpoint")))
			Expect(objects.Objects()).To(BeEmpty())
		})

		It("refuses to store a checkpoint for another job, writing nothing", func() {
			err := cp.Store(ctx, refOf("job-2", classify), []byte("{}"))
			Expect(err).To(MatchError(ContainSubstring(`job "job-2" is not job job-1`)))
			Expect(objects.Objects()).To(BeEmpty())
		})

		It("retries a failed write attestation.storage_retries times, then fails with an error that is not corrupt", func() {
			failing := &failingPutStorage{Fake: objects}
			c := serviceOver(failing).CacheCheckpointsFor(job, att, classify)
			ref := refOf("job-1", classify)
			signed, err := c.Sign(ctx, ref, digest)
			Expect(err).NotTo(HaveOccurred())

			err = c.Store(ctx, ref, signed)
			Expect(err).To(MatchError(pipeline.ErrAttestationStorage))
			Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
			Expect(failing.puts.Load()).To(BeEquivalentTo(3))
			Expect(counted("store", "error")).To(BeEquivalentTo(1))
		})

		It("stops retrying once the context is done", func() {
			failing := &failingPutStorage{Fake: objects}
			c := serviceOver(failing).CacheCheckpointsFor(job, att, classify)
			ref := refOf("job-1", classify)
			signed, err := c.Sign(ctx, ref, digest)
			Expect(err).NotTo(HaveOccurred())
			cancelled, cancel := context.WithCancel(ctx)
			cancel()

			err = c.Store(cancelled, ref, signed)
			Expect(err).To(MatchError(ContainSubstring("object store unavailable")))
			Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
			Expect(failing.puts.Load()).To(BeEquivalentTo(1), "no retry once the context is done")
		})
	})

	Describe("Verified", func() {
		It("finds nothing, and no error, for a producer without checkpoints", func() {
			vc, err := cp.Verified(ctx, classify, []string{"job-0"})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(BeEmpty())
			Expect(vc.Untrusted).To(BeEmpty())
		})

		It("returns the signed attempts of the job itself and of other producers, for the analyzer asked only", func() {
			own := refOf("job-1", classify)
			seal(cp, own, digest)
			other := refOf("job-0", classify)
			seal(svc.CacheCheckpointsFor(&pipeline.Job{JobID: "job-0", TenantID: tenantA, Config: []byte(`{}`)}, att, classify), other, digest)
			seal(cp, refOf("job-1", "embedding"), digest)

			vc, err := cp.Verified(ctx, classify, []string{"job-0", "job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(Equal(map[resultcache.AttemptRef]string{own: digest, other: digest}))
			Expect(counted("verify", "ok")).To(BeEquivalentTo(1))
			span := telemetrytest.FindSpan(tp.GetSpans(), "pipeline.CacheCheckpoints.verify")
			Expect(span).NotTo(BeNil())
			jobAttr, ok := telemetrytest.SpanAttribute(span, "job.id")
			Expect(ok).To(BeTrue())
			Expect(jobAttr.AsString()).To(Equal("job-1"))
			countAttr, ok := telemetrytest.SpanAttribute(span, "producers.count")
			Expect(ok).To(BeTrue())
			Expect(countAttr.AsInt64()).To(BeEquivalentTo(2))
		})

		It("ignores objects that are not named after a canonical attempt, such as a crashed write's temporary file", func() {
			ref := refOf("job-1", classify)
			seal(cp, ref, digest)
			raw := objects.Objects()[pipeline.CheckpointKey(tenantA, "job-1", classify, ref.Attempt)]
			dir := strings.TrimSuffix(pipeline.CheckpointKey(tenantA, "job-1", classify, ref.Attempt), ref.Attempt+".json")
			Expect(objects.Put(ctx, dir+".tmp-123", bytes.NewReader(raw))).To(Succeed())
			Expect(objects.Put(ctx, dir+"not-a-uuid.json", bytes.NewReader(raw))).To(Succeed())

			vc, err := cp.Verified(ctx, classify, []string{"job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(HaveLen(1))
		})

		It("leaves out a checkpoint signed by a key it does not trust, naming where it is and its key IDs, without logging it", func() {
			attempt := uuid.NewString()
			key := plant(otherGen, attempt, pipeline.CacheCheckpointStep, jobMaterial,
				[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, attempt), Digest: digest}})

			vc, err := cp.Verified(ctx, classify, []string{"job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(BeEmpty())
			Expect(vc.Untrusted).To(Equal([]analysis.UntrustedCheckpoint{{Location: key, KeyIDs: []string{otherKeyID}}}))
			Expect(logs.String()).To(BeEmpty(), "the engine logs untrusted keys")
		})

		It("fails as corrupt when a checkpoint's signature does not verify under the trusted key", func() {
			ref := refOf("job-1", classify)
			seal(cp, ref, digest)
			key := pipeline.CheckpointKey(tenantA, "job-1", classify, ref.Attempt)
			raw := objects.Objects()[key]
			// Sign a different digest into the payload, keeping the signature
			// (and its trusted key ID) as it was.
			var env map[string]any
			Expect(json.Unmarshal(raw, &env)).To(Succeed())
			payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(payload)).To(ContainSubstring(digest))
			env["payload"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(payload), digest, "sha256:"+strings.Repeat("b", 64), 1)))
			tampered, err := json.Marshal(env)
			Expect(err).NotTo(HaveOccurred())
			Expect(objects.Put(ctx, key, bytes.NewReader(tampered))).To(Succeed())

			_, err = cp.Verified(ctx, classify, []string{"job-1"})
			Expect(err).To(MatchError(resultcache.ErrCorrupt))
			Expect(err).To(MatchError(ContainSubstring(key)))
			Expect(objects.Objects()[key]).To(Equal(tampered), "verification changes nothing")
			Expect(counted("verify", "corrupt")).To(BeEquivalentTo(1))
		})

		DescribeTable("fails as corrupt when a checkpoint signs a subject other than where it is stored",
			func(build func(attempt string) (string, []attestation.Artifact, []attestation.Artifact), want string) {
				attempt := uuid.NewString()
				step, materials, products := build(attempt)
				key := plant(trustedGen, attempt, step, materials, products)

				vc, err := cp.Verified(ctx, classify, []string{"job-1"})
				Expect(err).To(MatchError(resultcache.ErrCorrupt))
				Expect(err).To(MatchError(And(ContainSubstring(key), ContainSubstring(want))))
				Expect(vc.Signed).To(BeEmpty())
			},
			Entry("another tenant", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI("tenant-b", "job-1", classify, a), Digest: digest}}
			}, "result-cache:tenant-b/job-1/classify/"),
			Entry("another job", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-2", classify, a), Digest: digest}}
			}, "result-cache:tenant-a/job-2/classify/"),
			Entry("another analyzer", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", "embedding", a), Digest: digest}}
			}, "result-cache:tenant-a/job-1/embedding/"),
			Entry("another attempt", func(string) (string, []attestation.Artifact, []attestation.Artifact) {
				other := uuid.NewString()
				return pipeline.CacheCheckpointStep, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, other), Digest: digest}}
			}, "result-cache:tenant-a/job-1/classify/"),
			Entry("another step", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return classify, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, a), Digest: digest}}
			}, `it is signed for step "classify"`),
			Entry("no product", func(string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial, nil
			}, "it signs 0 products, not one"),
			Entry("an extra product", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial, []attestation.Artifact{
					{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, a), Digest: digest},
					{URI: "extra", Digest: digest}}
			}, "it signs 2 products, not one"),
			Entry("a malformed digest", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, jobMaterial,
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, a), Digest: "sha256:zz"}}
			}, `its digest "sha256:zz" is not sha256:<64 hex>`),
			Entry("no job material", func(a string) (string, []attestation.Artifact, []attestation.Artifact) {
				return pipeline.CacheCheckpointStep, []attestation.Artifact{{URI: "job:tenant-a/job-2", Digest: "sha256:cfg"}},
					[]attestation.Artifact{{URI: pipeline.CheckpointURI(tenantA, "job-1", classify, a), Digest: digest}}
			}, `it does not name job "job:tenant-a/job-1" as a material`),
		)

		// A storage writer could plant a huge object under a checkpoint name;
		// it must not exhaust the daemon's memory or, through Verify's
		// temporary file, its disk.
		DescribeTable("bounds the checkpoint it reads",
			func(extra int, refused bool) {
				ref := refOf("job-1", classify)
				seal(cp, ref, digest)
				key := pipeline.CheckpointKey(tenantA, "job-1", classify, ref.Attempt)
				raw := objects.Objects()[key]
				// JSON allows trailing whitespace, so the padded checkpoint
				// is still the genuine, verifying envelope.
				padded := append(slices.Clone(raw), bytes.Repeat([]byte(" "), pipeline.MaxAttestationObjectBytes-len(raw)+extra)...)
				Expect(objects.Put(ctx, key, bytes.NewReader(padded))).To(Succeed())
				verifies := counter.verifies.Load()

				vc, err := cp.Verified(ctx, classify, []string{"job-1"})
				if !refused {
					Expect(err).NotTo(HaveOccurred())
					Expect(vc.Signed).To(Equal(map[resultcache.AttemptRef]string{ref: digest}))
					return
				}
				Expect(err).To(MatchError(resultcache.ErrCorrupt))
				Expect(err).To(MatchError(And(ContainSubstring(key), ContainSubstring("larger than the 1048576-byte limit"))))
				Expect(counter.verifies.Load()).To(Equal(verifies), "the oversized checkpoint was not verified, so no temporary file was written")
				Expect(objects.Objects()[key]).To(Equal(padded))
			},
			Entry("a checkpoint of exactly the limit is read and trusted", 0, false),
			Entry("a checkpoint one byte over the limit is corrupt, unread", 1, true),
		)

		It("fails as corrupt when another job's genuine checkpoint is copied under this job", func() {
			other := refOf("job-0", classify)
			seal(svc.CacheCheckpointsFor(&pipeline.Job{JobID: "job-0", TenantID: tenantA, Config: []byte(`{}`)}, att, classify), other, digest)
			raw := objects.Objects()[pipeline.CheckpointKey(tenantA, "job-0", classify, other.Attempt)]
			Expect(objects.Put(ctx, pipeline.CheckpointKey(tenantA, "job-1", classify, other.Attempt), bytes.NewReader(raw))).To(Succeed())

			_, err := cp.Verified(ctx, classify, []string{"job-1"})
			Expect(err).To(MatchError(resultcache.ErrCorrupt))
		})

		DescribeTable("fails with an error that is not corrupt when storage cannot be read",
			func(break_ func(*storagetest.Fake)) {
				seal(cp, refOf("job-1", classify), digest)
				break_(objects)
				_, err := cp.Verified(ctx, classify, []string{"job-1"})
				Expect(err).To(MatchError(ContainSubstring("object store unavailable")))
				Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
				Expect(counted("verify", "error")).To(BeEquivalentTo(1))
			},
			Entry("listing fails", func(f *storagetest.Fake) {
				f.ListFunc = func(context.Context, string) ([]storage.ObjectMetadata, error) {
					return nil, errors.New("object store unavailable")
				}
			}),
			Entry("reading fails", func(f *storagetest.Fake) {
				f.GetFunc = func(context.Context, string) (io.ReadCloser, error) {
					return nil, errors.New("object store unavailable")
				}
			}),
		)

		It("leaves out a checkpoint deleted between listing and reading", func() {
			seal(cp, refOf("job-1", classify), digest)
			objects.GetFunc = func(context.Context, string) (io.ReadCloser, error) { return nil, storage.ErrNotFound }

			vc, err := cp.Verified(ctx, classify, []string{"job-1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(BeEmpty())
		})

		It("trusts nothing of a producer whose ID cannot be a key segment, listing nothing for it", func() {
			var listed []string
			objects.ListFunc = func(_ context.Context, prefix string) ([]storage.ObjectMetadata, error) {
				listed = append(listed, prefix)
				return nil, nil
			}
			vc, err := cp.Verified(ctx, classify, []string{"../job-1", "a/b", "..", "."})
			Expect(err).NotTo(HaveOccurred())
			Expect(vc.Signed).To(BeEmpty())
			Expect(listed).To(BeEmpty())
		})

		It("fails with an error that is not corrupt for a context in another tenant or without one", func() {
			other, err := tenant.WithTenant(context.Background(), "tenant-b")
			Expect(err).NotTo(HaveOccurred())
			_, err = cp.Verified(other, classify, []string{"job-1"})
			Expect(err).To(MatchError(ContainSubstring(`the context's tenant "tenant-b" is not the job's tenant`)))
			Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
			_, err = cp.Verified(context.Background(), classify, []string{"job-1"})
			Expect(err).To(HaveOccurred())
			Expect(err).NotTo(MatchError(resultcache.ErrCorrupt))
		})
	})
})
