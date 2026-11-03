package pipeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// cacheCheckpointStep is the in-toto step name of a result-cache checkpoint
// link. No analyzer or pipeline step has it, so a checkpoint is never taken
// for a link of the job's attestation chain: VerifyChain refuses a link for
// an undeclared step, and loadStoredLinks checks each link's step.
const cacheCheckpointStep = "result-cache-checkpoint"

// checkpointDigestPattern is the form of a manifest digest
// (resultcache.Manifest.Digest).
var checkpointDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// cacheCheckpoints is analysis.CacheCheckpoints for one attested job
// (docs/dev/design-principles.md, "LLM Result Reuse"). A checkpoint is an in-toto link signed by the job's tenant
// attestor: step cacheCheckpointStep, the job (jobSubject) as its material,
// and one product, checkpointURI(tenant, job, analyzer, attempt) with the
// attempt's manifest digest. That product binds the tenant, job, analyzer,
// attempt and digest; it is stored at checkpointKey of the same four, and
// Verified accepts a checkpoint only where its product says it belongs.
type cacheCheckpoints struct {
	s         *Service
	job       *Job
	attestor  *Attestor
	subject   attestation.Artifact
	analyzers map[string]bool

	// mu guards stored, the digest each checkpoint Store stored signs:
	// analyzers of one DAG level seal concurrently. Only a stored
	// checkpoint is ever pinned in a link.
	mu     sync.Mutex
	stored map[resultcache.AttemptRef]string
}

var _ analysis.CacheCheckpoints = (*cacheCheckpoints)(nil)

// newCacheCheckpoints returns the checkpoints of job, signed and verified
// with attestor, for the analyzers named.
func (s *Service) newCacheCheckpoints(job *Job, attestor *Attestor, analyzers []string) *cacheCheckpoints {
	names := make(map[string]bool, len(analyzers))
	for _, name := range analyzers {
		names[name] = true
	}
	return &cacheCheckpoints{s: s, job: job, attestor: attestor, subject: jobSubject(job), analyzers: names,
		stored: map[resultcache.AttemptRef]string{}}
}

// checkpointDir is the object storage prefix of a job's checkpoints of one
// analyzer, beside the job's attestation links.
func checkpointDir(tenantID, jobID, analyzerName string) string {
	return fmt.Sprintf("attestations/%s/%s/cache-checkpoints/%s/", tenantID, jobID, analyzerName)
}

// checkpointKey is the object storage key of one attempt's checkpoint.
func checkpointKey(tenantID, jobID, analyzerName, attempt string) string {
	return checkpointDir(tenantID, jobID, analyzerName) + attempt + ".json"
}

// checkpointURI is the URI of a checkpoint's product: the attempt it vouches
// for.
func checkpointURI(tenantID, jobID, analyzerName, attempt string) string {
	return fmt.Sprintf("result-cache:%s/%s/%s/%s", tenantID, jobID, analyzerName, attempt)
}

// canonicalAttempt reports whether attempt is a UUID in its canonical form,
// as the engine generates them, which keeps it a safe single path segment.
func canonicalAttempt(attempt string) bool {
	u, err := uuid.Parse(attempt)
	return err == nil && u.String() == attempt
}

// pathSegment reports whether id can be one segment of an object key.
func pathSegment(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// checkRef refuses to sign or store a checkpoint for anything but an
// attempt of this job's analyzers in its own tenant.
func (c *cacheCheckpoints) checkRef(ctx context.Context, ref resultcache.AttemptRef) (string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}
	var problems []string
	if tenantID != c.job.TenantID {
		problems = append(problems, fmt.Sprintf("the context's tenant %q is not the job's tenant", tenantID))
	}
	if ref.JobID != c.job.JobID {
		problems = append(problems, fmt.Sprintf("job %q is not job %s", ref.JobID, c.job.JobID))
	}
	if !c.analyzers[ref.Analyzer] {
		problems = append(problems, fmt.Sprintf("analyzer %q is not one of the job's", ref.Analyzer))
	}
	if !canonicalAttempt(ref.Attempt) {
		problems = append(problems, fmt.Sprintf("attempt %q is not a canonical UUID", ref.Attempt))
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("refusing the cache checkpoint: %s", strings.Join(problems, "; "))
	}
	return tenantID, nil
}

// Sign implements analysis.CacheCheckpoints. It only signs: it runs under
// the job row lock.
func (c *cacheCheckpoints) Sign(ctx context.Context, ref resultcache.AttemptRef, digest string) (_ []byte, err error) {
	ctx, finish := c.s.startCheckpointOp(ctx, "sign", c.job.JobID, ref.Analyzer)
	defer func() { finish(err) }()
	tenantID, err := c.checkRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !checkpointDigestPattern.MatchString(digest) {
		return nil, fmt.Errorf("refusing the cache checkpoint: digest %q is not sha256:<64 hex>", digest)
	}
	link, err := c.attestor.Generator.CreateLink(ctx, cacheCheckpointStep, []attestation.Artifact{c.subject},
		[]attestation.Artifact{{URI: checkpointURI(tenantID, ref.JobID, ref.Analyzer, ref.Attempt), Digest: digest}})
	if err != nil {
		return nil, fmt.Errorf("signing the cache checkpoint of job %s analyzer %q attempt %s: %w",
			ref.JobID, ref.Analyzer, ref.Attempt, err)
	}
	return link.Raw, nil
}

// Store implements analysis.CacheCheckpoints, retrying a failed write as
// every attestation write is retried (attestation.storage_retries and
// storage_retry_delay); it runs after the job row lock is released. It
// first verifies that signed is ref's checkpoint, under the job's key, and
// remembers the digest it signs for the analyzer's link.
func (c *cacheCheckpoints) Store(ctx context.Context, ref resultcache.AttemptRef, signed []byte) (err error) {
	ctx, finish := c.s.startCheckpointOp(ctx, "store", c.job.JobID, ref.Analyzer)
	defer func() { finish(err) }()
	tenantID, err := c.checkRef(ctx, ref)
	if err != nil {
		return err
	}
	// The digest remembered for the link is the one the stored bytes sign,
	// read back from them, so it cannot differ from what is stored. An
	// envelope that is not ref's checkpoint is a caller bug, not tampering.
	key := checkpointKey(tenantID, ref.JobID, ref.Analyzer, ref.Attempt)
	digest, err := c.verifyCheckpoint(ctx, signed, key, tenantID, ref.JobID, ref.Analyzer, ref.Attempt)
	if err != nil {
		return fmt.Errorf("refusing to store the cache checkpoint of analyzer %q attempt %s: it is not that attempt's checkpoint: %v",
			ref.Analyzer, ref.Attempt, err)
	}
	what := fmt.Sprintf("cache checkpoint of analyzer %q attempt %s", ref.Analyzer, ref.Attempt)
	if err := c.s.putAttestation(ctx, c.job, what, key, signed); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored[ref] = digest
	return nil
}

// storedDigest returns the manifest digest of ref's checkpoint if this
// run signed and stored it.
func (c *cacheCheckpoints) storedDigest(ref resultcache.AttemptRef) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	digest, ok := c.stored[ref]
	return digest, ok
}

// Verified implements analysis.CacheCheckpoints. It reads every checkpoint
// stored under each producer's checkpointDir for analyzerName and verifies
// it with the job's attestor. Only objects named "<canonical UUID>.json"
// are read: anything else there (such as a temporary file a crashed write
// left) vouches for nothing and is ignored. It neither logs nor counts the
// checkpoints of an untrusted key; the engine does.
func (c *cacheCheckpoints) Verified(ctx context.Context, analyzerName string, producers []string) (_ analysis.VerifiedCheckpoints, err error) {
	ctx, finish := c.s.startCheckpointOp(ctx, "verify", c.job.JobID, analyzerName)
	defer func() { finish(err) }()
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("producers.count", len(producers)))
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return analysis.VerifiedCheckpoints{}, err
	}
	if tenantID != c.job.TenantID {
		return analysis.VerifiedCheckpoints{}, fmt.Errorf("verifying cache checkpoints: the context's tenant %q is not the job's tenant", tenantID)
	}
	if !c.analyzers[analyzerName] {
		return analysis.VerifiedCheckpoints{}, fmt.Errorf("verifying cache checkpoints: analyzer %q is not one of the job's", analyzerName)
	}
	out := analysis.VerifiedCheckpoints{Signed: map[resultcache.AttemptRef]string{}}
	for _, producer := range producers {
		// A producer ID that cannot be a key segment has no checkpoint, so
		// none of its attempts is trusted.
		if !pathSegment(producer) {
			continue
		}
		dir := checkpointDir(tenantID, producer, analyzerName)
		objects, err := c.s.storage.List(ctx, dir)
		if err != nil {
			return analysis.VerifiedCheckpoints{}, fmt.Errorf("listing the cache checkpoints of job %s at %q: %w", producer, dir, err)
		}
		for _, obj := range objects {
			name, ok := strings.CutPrefix(obj.Key, dir)
			if !ok {
				continue
			}
			attempt, ok := strings.CutSuffix(name, ".json")
			if !ok || !canonicalAttempt(attempt) {
				continue
			}
			raw, err := c.s.readAttestation(ctx, obj.Key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if errors.Is(err, errAttestationTooLarge) {
				return analysis.VerifiedCheckpoints{}, fmt.Errorf("%w: the cache checkpoint at %q is larger than the %d-byte limit, "+
					"so it was not read. A checkpoint is about 1 KB, so the object was replaced after signing; "+
					"check who can write to object storage", resultcache.ErrCorrupt, obj.Key, maxAttestationObjectBytes)
			}
			if err != nil {
				return analysis.VerifiedCheckpoints{}, fmt.Errorf("reading the cache checkpoint at %q: %w", obj.Key, err)
			}
			digest, err := c.verifyCheckpoint(ctx, raw, obj.Key, tenantID, producer, analyzerName, attempt)
			if untrusted, ok := errors.AsType[*attestation.UntrustedKeyError](err); ok {
				out.Untrusted = append(out.Untrusted, analysis.UntrustedCheckpoint{Location: obj.Key, KeyIDs: untrusted.KeyIDs})
				continue
			}
			if err != nil {
				return analysis.VerifiedCheckpoints{}, err
			}
			out.Signed[resultcache.AttemptRef{JobID: producer, Analyzer: analyzerName, Attempt: attempt}] = digest
		}
	}
	return out, nil
}

// errInvalidAttempt reports an attempt that is not a canonical UUID, so it
// names no checkpoint.
var errInvalidAttempt = errors.New("the attempt is not a canonical UUID")

// errCheckpointUnreadable marks a failure to read a checkpoint from object
// storage, as opposed to one that was read and does not verify.
var errCheckpointUnreadable = errors.New("the cache checkpoint could not be read")

// ownCheckpoint reads the checkpoint of this job's attempt of analyzerName
// and returns the manifest digest it signs. attempt comes from the
// database and is not trusted: it must be a canonical UUID
// (errInvalidAttempt). A missing checkpoint wraps storage.ErrNotFound; one
// signed by an untrusted key is an *attestation.UntrustedKeyError; a read
// failure wraps errCheckpointUnreadable; any other error means the object
// read does not verify.
func (c *cacheCheckpoints) ownCheckpoint(ctx context.Context, analyzerName, attempt string) (string, error) {
	if !canonicalAttempt(attempt) {
		return "", fmt.Errorf("%w: %q", errInvalidAttempt, attempt)
	}
	key := checkpointKey(c.job.TenantID, c.job.JobID, analyzerName, attempt)
	raw, err := c.s.readAttestation(ctx, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return "", fmt.Errorf("the cache checkpoint at %q: %w", key, err)
	case errors.Is(err, errAttestationTooLarge):
		return "", fmt.Errorf("%w: the cache checkpoint at %q is larger than the %d-byte limit, so it was not read",
			resultcache.ErrCorrupt, key, maxAttestationObjectBytes)
	case err != nil:
		return "", fmt.Errorf("%w: reading %q: %w", errCheckpointUnreadable, key, err)
	}
	return c.verifyCheckpoint(ctx, raw, key, c.job.TenantID, c.job.JobID, analyzerName, attempt)
}

// verifyCheckpoint verifies the checkpoint raw stored at key and returns
// the digest it signs. It returns an error wrapping
// attestation.ErrUntrustedKey for a checkpoint signed by a key the job's
// attestor does not trust; one wrapping resultcache.ErrCorrupt for a bad
// signature under the trusted key, or a signed subject other than where it
// is stored; and any other error otherwise.
func (c *cacheCheckpoints) verifyCheckpoint(ctx context.Context, raw []byte, key, tenantID, jobID, analyzerName, attempt string) (string, error) {
	verified, err := c.attestor.Generator.Verify(ctx, raw)
	switch {
	case errors.Is(err, attestation.ErrUntrustedKey):
		return "", err
	case errors.Is(err, attestation.ErrVerificationFailed):
		return "", fmt.Errorf("%w: the cache checkpoint at %q does not verify under the trusted key: %w", resultcache.ErrCorrupt, key, err)
	case err != nil:
		return "", fmt.Errorf("verifying the cache checkpoint at %q: %w", key, err)
	}
	var problems []string
	if verified.Step != cacheCheckpointStep {
		problems = append(problems, fmt.Sprintf("it is signed for step %q", verified.Step))
	}
	wantURI := checkpointURI(tenantID, jobID, analyzerName, attempt)
	var digest string
	switch {
	case len(verified.Products) != 1:
		problems = append(problems, fmt.Sprintf("it signs %d products, not one", len(verified.Products)))
	case verified.Products[0].URI != wantURI:
		problems = append(problems, fmt.Sprintf("it signs %q, not %q", verified.Products[0].URI, wantURI))
	case !checkpointDigestPattern.MatchString(verified.Products[0].Digest):
		problems = append(problems, fmt.Sprintf("its digest %q is not sha256:<64 hex>", verified.Products[0].Digest))
	default:
		digest = verified.Products[0].Digest
	}
	wantJob := jobURI(tenantID, jobID)
	if !slices.ContainsFunc(verified.Materials, func(a attestation.Artifact) bool { return a.URI == wantJob }) {
		problems = append(problems, fmt.Sprintf("it does not name job %q as a material", wantJob))
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("%w: the cache checkpoint at %q is not bound to where it is stored: %s",
			resultcache.ErrCorrupt, key, strings.Join(problems, "; "))
	}
	return digest, nil
}

// startCheckpointOp starts the span of one checkpoint operation (sign,
// store or verify) of jobID's checkpoints and returns a function that ends
// it, recording its outcome (ok, corrupt or error) and duration.
func (s *Service) startCheckpointOp(ctx context.Context, op, jobID, analyzerName string) (context.Context, func(error)) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CacheCheckpoints."+op)
	tenantID, _ := tenant.FromContext(ctx)
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("job.id", jobID),
		attribute.String("analyzer.name", analyzerName))
	start := time.Now()
	inFlight := s.checkpointsInFlight.Add(1)
	if s.checkpointGauge != nil {
		s.checkpointGauge.Record(ctx, inFlight)
	}
	return ctx, func(err error) {
		outcome := "ok"
		switch {
		case errors.Is(err, resultcache.ErrCorrupt):
			outcome = "corrupt"
		case err != nil:
			outcome = "error"
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, outcome)
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
		attrs := metric.WithAttributes(attribute.String("tenant.id", tenantID), attribute.String("operation", op),
			attribute.String("outcome", outcome))
		if s.checkpointOps != nil {
			s.checkpointOps.Add(ctx, 1, attrs)
		}
		if s.checkpointDuration != nil {
			s.checkpointDuration.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
		}
		inFlight := s.checkpointsInFlight.Add(-1)
		if s.checkpointGauge != nil {
			s.checkpointGauge.Record(ctx, inFlight)
		}
	}
}
