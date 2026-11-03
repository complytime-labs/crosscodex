package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// CacheCheckpoints signs and verifies a job's cache checkpoints (spec
// amendment 15): the only source of trust in a cached result. The pipeline
// implements it over pkg/attestation for the job's tenant. Both methods
// read the tenant from ctx.
type CacheCheckpoints interface {
	// Verified returns producers' checkpointed attempts of analyzer whose
	// signature verifies under a key the daemon trusts, each mapped to the
	// manifest digest it signed. No checkpoint, or one signed by a key the
	// daemon does not trust (rotated or unknown), leaves the attempt out;
	// the latter are counted in UntrustedKeys. An invalid signature under a
	// trusted key, or a signed subject naming another tenant, job,
	// analyzer or attempt than where it is stored, wraps
	// resultcache.ErrCorrupt. Every other failure must not wrap ErrCorrupt.
	Verified(ctx context.Context, analyzer string, producers []string) (VerifiedCheckpoints, error)
	// Seal signs a checkpoint whose subject binds the ctx tenant,
	// ref.JobID, ref.Analyzer, ref.Attempt and digest, and stores it beside
	// the job's attestation links. It runs inside Store.Checkpoint under the
	// job row lock: it must not call the result cache store or touch the
	// job row.
	Seal(ctx context.Context, ref resultcache.AttemptRef, digest string) error
}

// VerifiedCheckpoints is what CacheCheckpoints.Verified found.
type VerifiedCheckpoints struct {
	// Signed maps each trusted attempt to its signed manifest digest.
	Signed map[resultcache.AttemptRef]string
	// UntrustedKeys counts checkpoints left out because their signing key
	// is not trusted.
	UntrustedKeys int
}

// errSealFailed marks the error of CacheCheckpoints.Seal inside the error
// Store.Checkpoint returns, so a sealer failure is never classified as a
// record mismatch, whatever it wraps.
var errSealFailed = errors.New("the cache checkpoint could not be signed or stored")

// cachedTask is one task the stage may look up and store.
type cachedTask struct {
	request   analyzer.CacheRequest
	model     CacheModel
	lookupKey string
	source    string
	target    string
}

// stageCache is one analyzer's use of the cache in one execution: one
// attempt, and the engine's in-process record of what it wrote, which is
// what Checkpoint compares the database with.
type stageCache struct {
	name     string
	tenantID string
	jobID    string
	attempt  string
	settings CacheSettings
	// cacheable: the analyzer opted in and has an observed model, in a
	// mode that stores. analyzer is the opted-in analyzer.
	cacheable bool
	analyzer  analyzer.Cacheable
	byTask    map[string]cachedTask
	stats     resultcache.Stats
	hits      []resultcache.HitEntry
	writes    []resultcache.ManifestEntry
	// recorded: RecordStage stored hits and stats.
	recorded bool
	// uncertain: a Put or RecordStage failed in a way that may still have
	// committed, so a record mismatch is not evidence of tampering.
	uncertain bool
	attrs     metric.MeasurementOption
}

// records reports whether the stage makes store calls at all: never in
// ModeDisabled, the tenant's kill switch.
func (sc *stageCache) records() bool {
	return sc != nil && sc.settings.Mode != resultcache.ModeDisabled
}

func (sc *stageCache) lookups() bool {
	return sc != nil && sc.cacheable && sc.settings.Mode == resultcache.ModeEnabled && len(sc.byTask) > 0
}

func (sc *stageCache) puts() bool { return sc != nil && sc.cacheable && len(sc.byTask) > 0 }

// ref names the stage's attempt.
func (sc *stageCache) ref() resultcache.AttemptRef {
	return resultcache.AttemptRef{JobID: sc.jobID, Analyzer: sc.name, Attempt: sc.attempt}
}

// expected is the attempt's record as the engine made it: the writes Put
// accepted and, once RecordStage succeeded, the hits and the stats row.
func (sc *stageCache) expected() resultcache.Manifest {
	m := resultcache.Manifest{TenantID: sc.tenantID, JobID: sc.jobID, Analyzer: sc.name, Attempt: sc.attempt,
		Entries: slices.Clone(sc.writes)}
	if sc.recorded {
		for _, h := range sc.hits {
			m.Entries = append(m.Entries, resultcache.ManifestEntry{TaskID: h.TaskID, Role: resultcache.RoleHit,
				CacheKey: h.CacheKey, ResultDigest: h.ResultDigest, ProducerJobID: h.ProducerJobID})
		}
		m.Stats = []resultcache.Stats{sc.stats}
	}
	return m
}

// definite reports a store refusal that wrote nothing.
func definite(err error) bool {
	return errors.Is(err, resultcache.ErrInvalidEntry) || errors.Is(err, resultcache.ErrJobNotRunning) ||
		errors.Is(err, resultcache.ErrUnknownJob)
}

// tasksNotCached says that n tasks were not cached, and why.
func tasksNotCached(n int, why string) string {
	if n == 1 {
		return "1 task was not cached: " + why
	}
	return fmt.Sprintf("%d tasks were not cached: %s", n, why)
}

// validateCache refuses settings the engine cannot honour.
func (e *Engine) validateCache(c CacheSettings) error {
	switch c.Mode {
	case "", resultcache.ModeDisabled:
		return nil
	case resultcache.ModeEnabled, resultcache.ModeBypassed:
	default:
		return fmt.Errorf("%w: mode %q is not enabled, bypassed or disabled", ErrInvalidCacheRequest, c.Mode)
	}
	if e.cacheStore == nil {
		return fmt.Errorf("%w: the request sets cache mode %q", ErrResultCacheNotWired, c.Mode)
	}
	var problems []string
	if c.Checkpoints == nil {
		problems = append(problems, "no checkpoint signer is set, so no result could ever be trusted")
	}
	if c.NoCacheCommand == "" {
		problems = append(problems, "no --no-cache command is set for the cache errors to quote")
	}
	if c.Mode == resultcache.ModeEnabled && c.LookupBatchSize < 1 {
		problems = append(problems, fmt.Sprintf("lookup batch size %d must be at least 1", c.LookupBatchSize))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: mode %q: %s", ErrInvalidCacheRequest, c.Mode, strings.Join(problems, "; "))
	}
	return nil
}

// planCache decides how analyzer name uses the cache for tasks, and which
// tasks it may look up and store. origins maps each per-control task to
// the control it was generated from. It returns nil when the request has
// no cache mode.
func (e *Engine) planCache(ctx context.Context, name string, ra analyzer.RegisteredAnalyzer, req ExecutionRequest,
	tasks []analyzer.Task, origins map[string]*pb.Control) (*stageCache, error) {
	if req.Cache.Mode == "" {
		return nil, nil
	}
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoTenant, err)
	}
	sc := &stageCache{
		name: name, tenantID: tenantID, jobID: req.JobID, attempt: uuid.NewString(), settings: req.Cache,
		byTask: map[string]cachedTask{},
		stats:  resultcache.Stats{Mode: req.Cache.Mode, Models: map[string]resultcache.ModelIdentity{}},
		attrs:  metric.WithAttributes(attribute.String("tenant.id", tenantID), attribute.String("analyzer.name", name)),
	}
	if req.Cache.Mode == resultcache.ModeDisabled {
		sc.stats.Reason = "analysis.result_cache.enabled is false for this tenant, so nothing was looked up or stored"
		return sc, nil
	}
	c, ok := analyzer.AsCacheable(ra)
	if !ok {
		sc.stats.Mode = resultcache.ModeSkipped
		sc.stats.Reason = "the analyzer does not opt in to result reuse (it does not implement analyzer.Cacheable)"
		return sc, nil
	}
	uses := map[string]analyzer.ModelUse{}
	var reasons []string
	observed := false
	for _, use := range c.CacheModels() {
		uses[use.Alias] = use
		cm, probed := req.Cache.Models[use]
		if !probed {
			reasons = append(reasons, fmt.Sprintf("alias %q was not probed for this job", use.Alias))
			continue
		}
		sc.stats.Models[cm.Alias] = resultcache.ModelIdentity{Observed: cm.Observed, Revision: cm.Revision}
		if cm.Observed == "" {
			reasons = append(reasons, cm.Reason)
			continue
		}
		observed = true
	}
	slices.Sort(reasons)
	reasons = slices.Compact(reasons)
	if !observed {
		sc.stats.Mode = resultcache.ModeSkipped
		if len(reasons) == 0 {
			reasons = []string{"the analyzer names no model"}
		}
		sc.stats.Reason = strings.Join(reasons, "; ")
		return sc, nil
	}
	if req.Cache.Mode == resultcache.ModeBypassed {
		reasons = append([]string{"the job was started with --no-cache, so nothing was looked up"}, reasons...)
	}
	sc.cacheable = true
	sc.analyzer = c

	// Entries are keyed by task ID, so a task ID that repeats in the stage
	// cannot be recorded unambiguously.
	idCount := map[string]int{}
	for _, t := range tasks {
		idCount[t.TaskID]++
	}
	duplicates, uncataloged := 0, 0
	for _, t := range tasks {
		if t.PreBuiltResult {
			continue
		}
		if idCount[t.TaskID] > 1 {
			duplicates++
			continue
		}
		cr, ok := c.CacheIdentity(t)
		if !ok {
			continue
		}
		use, ok := uses[cr.Identity.ModelAlias]
		if !ok {
			continue
		}
		cm := req.Cache.Models[use]
		if cm.Observed == "" {
			continue
		}
		source, target, ok := taskCatalogs(cr, origins[t.TaskID], req.Cache.ControlCatalogs)
		if !ok {
			uncataloged++
			continue
		}
		sc.byTask[t.TaskID] = cachedTask{request: cr, model: cm, source: source, target: target,
			lookupKey: resultcache.Key(name, cr.Identity, cm.Observed, cm.Revision)}
	}
	if uncataloged > 0 {
		reasons = append(reasons, tasksNotCached(uncataloged, "no catalog is known for its controls"))
	}
	if duplicates > 0 {
		reasons = append(reasons, tasksNotCached(duplicates, "their task ID is not unique in the stage"))
	}
	sc.stats.Reason = strings.Join(reasons, "; ")
	return sc, nil
}

// taskCatalogs returns the catalogs of the controls a task's request
// carries. A per-control task takes the catalog of the control it was
// generated from (origin), which the analyzer may name by ControlId or by
// Identifier (artifacts does); any other task looks its controls up by
// ControlId in catalogs.
func taskCatalogs(cr analyzer.CacheRequest, origin *pb.Control, catalogs map[string]string) (string, string, bool) {
	if origin != nil && origin.GetCatalogId() != "" && cr.SourceControlID == cr.TargetControlID &&
		(cr.SourceControlID == origin.GetControlId() || cr.SourceControlID == origin.GetIdentifier()) {
		return origin.GetCatalogId(), origin.GetCatalogId(), true
	}
	source, target := catalogs[cr.SourceControlID], catalogs[cr.TargetControlID]
	return source, target, source != "" && target != ""
}

// trackInFlight moves the in-flight gauge by delta.
func (e *Engine) trackInFlight(ctx context.Context, delta int64) {
	n := e.inFlight.Add(delta)
	if e.cacheInFlight != nil {
		e.cacheInFlight.Record(ctx, n)
	}
}

func (e *Engine) countIntegrityFailures(ctx context.Context, sc *stageCache, n int, reason string) {
	if e.cacheIntegrityFailures != nil && n > 0 {
		e.cacheIntegrityFailures.Add(ctx, int64(n), metric.WithAttributes(attribute.String("tenant.id", sc.tenantID),
			attribute.String("analyzer.name", sc.name), attribute.String("reason", reason)))
	}
}

// lookupCached reads the stage's keys in batches and returns the verified,
// decoded cached result per task, recording hit entries in sc. For each
// batch it learns the producers, loads and verifies their checkpoints
// before the snapshot that relies on them, reads the snapshot, and
// verifies every row in it against the signed attempts it was read with.
// A failed read is a result_cache_unavailable StageError; any row that
// fails verification or decoding is a result_cache_corrupt StageError, and
// none is used.
func (e *Engine) lookupCached(ctx context.Context, sc *stageCache) (map[string]*structpb.Struct, error) {
	ctx, span := telemetry.StartSpan(e.tracer, ctx, "analysis.ResultCacheLookup")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", sc.tenantID), attribute.String("analyzer.name", sc.name),
		attribute.String("job.id", sc.jobID))

	keySet := map[string]bool{}
	for _, ct := range sc.byTask {
		keySet[ct.lookupKey] = true
	}
	keys := slices.Sorted(maps.Keys(keySet))
	e.trackInFlight(ctx, int64(len(keys)))
	defer e.trackInFlight(ctx, -int64(len(keys)))
	begun := time.Now()
	// Every lookup is counted and timed, the failed ones too.
	defer func() {
		if e.cacheLookups != nil {
			e.cacheLookups.Add(ctx, int64(len(keys)), sc.attrs)
		}
		if e.cacheLookupDuration != nil {
			e.cacheLookupDuration.Record(ctx, float64(time.Since(begun))/float64(time.Millisecond), sc.attrs)
		}
	}()

	// A producer's checkpoints are loaded once per stage, before the first
	// snapshot that needs them. A checkpoint it seals later is missing from
	// signed, so a row it stores under that attempt is a miss: never a
	// false hit, and never a false corrupt (only the storing attempt is
	// checked, and only when signed holds it).
	signed := map[resultcache.AttemptRef]string{}
	verified := map[string]bool{}
	found := map[string]resultcache.Hit{}
	decoded := map[string]*structpb.Struct{}
	for start := 0; start < len(keys); start += sc.settings.LookupBatchSize {
		batch := keys[start:min(start+sc.settings.LookupBatchSize, len(keys))]
		producers, err := e.cacheStore.Producers(ctx, sc.name, batch)
		if err != nil {
			return nil, e.cacheUnavailable(ctx, sc, len(keys), "reading the producers of cached results", err)
		}
		batchProducers := map[string]bool{}
		var unverified []string
		for _, p := range producers {
			batchProducers[p] = true
			if !verified[p] {
				verified[p] = true
				unverified = append(unverified, p)
			}
		}
		if len(unverified) > 0 {
			slices.Sort(unverified)
			unverified = slices.Compact(unverified)
			vc, err := sc.settings.Checkpoints.Verified(ctx, sc.name, unverified)
			if err != nil {
				if errors.Is(err, resultcache.ErrCorrupt) {
					e.countIntegrityFailures(ctx, sc, 1, "checkpoint")
					return nil, e.cacheCorrupt(ctx, sc, keysOf(producers, unverified), unverified, err)
				}
				return nil, e.cacheUnavailable(ctx, sc, len(keys), "loading the producers' cache checkpoints", err)
			}
			if vc.UntrustedKeys > 0 {
				e.logger.WarnContext(ctx, "cache checkpoints signed by a key the daemon no longer trusts were ignored, "+
					"so the results they cover are recomputed. Expected after a signing key rotation",
					"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "producer_jobs", unverified,
					"untrusted_checkpoints", vc.UntrustedKeys)
				e.countIntegrityFailures(ctx, sc, vc.UntrustedKeys, "untrusted_key")
			}
			maps.Copy(signed, vc.Signed)
		}
		// Verify sees exactly the attempts the snapshot was asked to read:
		// a producer that changed after Producers then has none, and its
		// row is a miss.
		batchSigned := map[resultcache.AttemptRef]string{}
		for ref, digest := range signed {
			if batchProducers[ref.JobID] {
				batchSigned[ref] = digest
			}
		}
		if len(batchSigned) == 0 {
			continue
		}
		refs := slices.SortedFunc(maps.Keys(batchSigned), func(a, b resultcache.AttemptRef) int {
			return strings.Compare(a.JobID+"\x00"+a.Attempt, b.JobID+"\x00"+b.Attempt)
		})
		snap, err := e.cacheStore.LookupVerifiable(ctx, sc.name, batch, refs)
		if err != nil {
			if errors.Is(err, resultcache.ErrCorrupt) {
				e.countIntegrityFailures(ctx, sc, 1, "snapshot")
				return nil, e.cacheCorrupt(ctx, sc, batch, slices.Sorted(maps.Keys(batchProducers)), err)
			}
			return nil, e.cacheUnavailable(ctx, sc, len(keys), "reading cached results", err)
		}
		var badKeys []string
		badProducers := map[string]bool{}
		var causes []error
		for _, key := range snap.Keys() {
			h, err := snap.Verify(key, batchSigned)
			if err == nil {
				var s *structpb.Struct
				if s, err = resultcache.DecodeResult(h.Result); err == nil {
					found[key] = h
					decoded[key] = s
					continue
				}
			}
			switch {
			case errors.Is(err, resultcache.ErrUntrusted):
			case errors.Is(err, resultcache.ErrCorrupt):
				badKeys = append(badKeys, key)
				// Verify returns no hit with its error; Producers named the
				// producer, unless the row appeared after it.
				if p := producers[key]; p != "" {
					badProducers[p] = true
				}
				causes = append(causes, err)
			default:
				e.logger.ErrorContext(ctx, "bug: the result cache snapshot could not verify a row it was asked for; "+
					"the engine passed attempts the snapshot did not read",
					"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "cache_key", key, "error", err)
				return nil, e.cacheUnavailable(ctx, sc, len(keys), "verifying cached results", err)
			}
		}
		if len(badKeys) > 0 {
			e.countIntegrityFailures(ctx, sc, len(badKeys), "result")
			return nil, e.cacheCorrupt(ctx, sc, badKeys, slices.Sorted(maps.Keys(badProducers)), errors.Join(causes...))
		}
	}
	results := map[string]*structpb.Struct{}
	for _, taskID := range slices.Sorted(maps.Keys(sc.byTask)) {
		ct := sc.byTask[taskID]
		s, ok := decoded[ct.lookupKey]
		if !ok {
			continue
		}
		// Tasks with the same request share a key; each gets its own copy.
		results[taskID] = proto.Clone(s).(*structpb.Struct)
		h := found[ct.lookupKey]
		sc.hits = append(sc.hits, resultcache.HitEntry{TaskID: taskID, CacheKey: ct.lookupKey,
			ResultDigest: resultcache.ResultDigest(h.Result), ProducerJobID: h.ProducerJobID})
	}
	if e.cacheHits != nil {
		e.cacheHits.Add(ctx, int64(len(results)), sc.attrs)
	}
	span.SetAttributes(attribute.Int("cache.keys", len(keys)), attribute.Int("cache.hits", len(results)))
	span.SetStatus(codes.Ok, "")
	return results, nil
}

// keysOf returns the keys in producers whose producer is one of jobs.
func keysOf(producers map[string]string, jobs []string) []string {
	var out []string
	for key, p := range producers {
		if slices.Contains(jobs, p) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// putResults stores every successful dispatched result of a cacheable task
// that the analyzer would use, and records each write Put accepted. A
// result is stored only when the model that answered is the alias's
// observed model, under the key the job looks up: a gateway fallback's
// result would otherwise be filed under a model this job's probe never saw.
// A refused or failed put is logged and counted and the stage goes on: its
// results are valid, and the write is not in the record, so it is never
// sealed.
func (e *Engine) putResults(ctx context.Context, sc *stageCache, results []analyzer.TaskResult) {
	for _, r := range results {
		ct, ok := sc.byTask[r.TaskID]
		if !ok || r.Error != nil {
			continue
		}
		s, ok := r.Result.(*structpb.Struct)
		if !ok || s == nil {
			continue
		}
		respModel := s.GetFields()["model"].GetStringValue()
		if respModel == "" {
			continue
		}
		if respModel != ct.model.Observed {
			e.putRefused(ctx, sc, r.TaskID, "model_mismatch",
				"the gateway answered with another model than the job's probe observed for the alias, so the result is not stored",
				"response_model", respModel, "observed_model", ct.model.Observed)
			continue
		}
		if err := sc.analyzer.CacheableResult(s); err != nil {
			e.putRefused(ctx, sc, r.TaskID, "unusable_result",
				"the result is one the analyzer would discard, so it is not stored and later jobs ask the model again",
				"reason", err.Error())
			continue
		}
		data, err := resultcache.EncodeResult(s)
		if err != nil {
			e.putFailed(ctx, sc, r.TaskID, err)
			continue
		}
		e.trackInFlight(ctx, 1)
		err = e.cacheStore.Put(ctx, resultcache.Entry{
			JobID: sc.jobID, Analyzer: sc.name, Attempt: sc.attempt, TaskID: r.TaskID, CacheKey: ct.lookupKey,
			Result: data, ResponseModel: respModel, SourceCatalogID: ct.source, TargetCatalogID: ct.target,
		})
		e.trackInFlight(ctx, -1)
		if err != nil {
			if !definite(err) {
				sc.uncertain = true
			}
			e.putFailed(ctx, sc, r.TaskID, err)
			continue
		}
		sc.writes = append(sc.writes, resultcache.ManifestEntry{TaskID: r.TaskID, Role: resultcache.RoleWrite,
			CacheKey: ct.lookupKey, ResultDigest: resultcache.ResultDigest(data)})
		if e.cachePuts != nil {
			e.cachePuts.Add(ctx, 1, sc.attrs)
		}
	}
}

// putRefused logs and counts a result putResults chose not to store.
func (e *Engine) putRefused(ctx context.Context, sc *stageCache, taskID, reason, msg string, attrs ...any) {
	e.logger.WarnContext(ctx, msg, append([]any{"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID,
		"task_id", taskID}, attrs...)...)
	if e.cachePutsRefused != nil {
		e.cachePutsRefused.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant.id", sc.tenantID),
			attribute.String("analyzer.name", sc.name), attribute.String("reason", reason)))
	}
}

func (e *Engine) putFailed(ctx context.Context, sc *stageCache, taskID string, err error) {
	e.logger.WarnContext(ctx, "a result could not be stored in the result cache; the job uses it, but later jobs cannot reuse it. "+
		"Check that the database is reachable",
		"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "attempt", sc.attempt, "task_id", taskID, "error", err)
	if e.cachePutErrors != nil {
		e.cachePutErrors.Add(ctx, 1, sc.attrs)
	}
}

// recordStage writes the stage's hit entries and stats row.
func (e *Engine) recordStage(ctx context.Context, sc *stageCache) error {
	slices.SortFunc(sc.hits, func(a, b resultcache.HitEntry) int { return strings.Compare(a.TaskID, b.TaskID) })
	err := e.cacheStore.RecordStage(ctx, resultcache.StageRecord{JobID: sc.jobID, Analyzer: sc.name, Attempt: sc.attempt,
		Hits: sc.hits, Stats: sc.stats})
	if err != nil {
		if !definite(err) {
			sc.uncertain = true
		}
		return err
	}
	sc.recorded = true
	return nil
}

// seal compares the database with the stage's record under the job lock
// and, when they match, has the checkpoint signed and stored.
func (e *Engine) seal(ctx context.Context, sc *stageCache) error {
	ctx, span := telemetry.StartSpan(e.tracer, ctx, "analysis.ResultCacheCheckpoint")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", sc.tenantID), attribute.String("analyzer.name", sc.name),
		attribute.String("job.id", sc.jobID), attribute.String("cache.attempt", sc.attempt))
	ref := sc.ref()
	err := e.cacheStore.Checkpoint(ctx, sc.expected(), func(ctx context.Context, digest string) error {
		if err := sc.settings.Checkpoints.Seal(ctx, ref, digest); err != nil {
			return fmt.Errorf("%w: %w", errSealFailed, err)
		}
		return nil
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "checkpoint failed")
		return err
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

// mismatch reports a Checkpoint refusal because the database does not hold
// what the stage recorded, rather than a failure to sign or store.
func mismatch(err error) bool {
	return !errors.Is(err, errSealFailed) && errors.Is(err, resultcache.ErrCorrupt)
}

// sealAfterFailure seals the writes of a stage that failed, so a retry
// reuses the results it paid for. It returns whether it sealed. A stage
// that wrote nothing has nothing to vouch for and is not sealed; one whose
// context was cancelled is left unsealed, which leaves its writes
// untrusted. A failure to seal is logged: the stage's own error stands.
func (e *Engine) sealAfterFailure(ctx context.Context, sc *stageCache) bool {
	if !sc.records() || len(sc.writes) == 0 {
		return false
	}
	if ctx.Err() != nil {
		e.logger.InfoContext(ctx, "the stage stopped because its job was cancelled or the daemon is stopping; "+
			"its result-cache writes were not sealed, so a later run recomputes them",
			"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "attempt", sc.attempt, "writes", len(sc.writes))
		return false
	}
	err := e.seal(ctx, sc)
	if err == nil {
		return true
	}
	if mismatch(err) {
		e.countIntegrityFailures(ctx, sc, 1, mismatchReason(sc))
		e.logger.ErrorContext(ctx, mismatchLog(sc), "analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID,
			"attempt", sc.attempt, "uncertain_write", sc.uncertain, "error", err)
		return false
	}
	e.logger.WarnContext(ctx, "the failed stage's result-cache writes could not be sealed, so a later run recomputes them",
		"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "attempt", sc.attempt, "error", err)
	return false
}

// mismatchReason is the integrity-failure reason of a record mismatch: one
// a write whose outcome was unknown may explain is not a tamper alarm.
func mismatchReason(sc *stageCache) string {
	if sc.uncertain {
		return "stage_record_uncertain"
	}
	return "stage_record"
}

// mismatchLog is the operator log line for a stage record the database
// does not match.
func mismatchLog(sc *stageCache) string {
	if sc.uncertain {
		return "the database's result-cache record for the stage run does not match what the stage recorded, so it was not sealed. " +
			"A write whose outcome was unknown (see the earlier put or stage-record warning) may have committed; " +
			"if the rows listed are not that write, someone holding app_user credentials wrote into a running job's record"
	}
	return "the database holds result-cache records for the stage run that the stage did not write, or lacks ones it did, " +
		"so it was not sealed. Someone holding app_user credentials wrote into a running job's record: investigate the rows listed"
}

func (e *Engine) cacheUnavailable(ctx context.Context, sc *stageCache, keyCount int, doing string, cause error) *StageError {
	e.logger.ErrorContext(ctx, "the result cache failed while "+doing+", so the stage stopped before any LLM call. "+
		"Check that the database and attestation storage are reachable and that the schema is current "+
		"(crosscodexd migrate with database.migration_dsn)",
		"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "keys", keyCount, "error", cause)
	return &StageError{Analyzer: sc.name, Sentinel: resultcache.ErrUnavailable, Cause: cause, Message: fmt.Sprintf(
		"Stage %q stopped before making any LLM calls: the result cache could not be read (error class result_cache_unavailable). "+
			"No results were written for this stage. Retry this job once the cache is reachable (crosscodex run retry %s), "+
			"or start a new job that skips the cache: %s. If this keeps happening, give your operator %s.",
		sc.name, sc.jobID, sc.settings.NoCacheCommand, TraceReference(ctx, sc.jobID))}
}

func (e *Engine) cacheCorrupt(ctx context.Context, sc *stageCache, keys, producers []string, cause error) *StageError {
	e.logger.ErrorContext(ctx, "cached results or their producers' checkpoints failed their integrity check, so the stage "+
		"stopped and used none of them. The rows or checkpoints were changed after their producing jobs signed them; "+
		"a job started with --no-cache replaces the cached results",
		"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "cache_keys", keys,
		"producer_jobs", producers, "error", cause)
	return &StageError{Analyzer: sc.name, Sentinel: resultcache.ErrCorrupt, Cause: cause, Message: fmt.Sprintf(
		"Stage %q stopped: a cached result failed its integrity check, so it was not used and no results were written for this stage. "+
			"Start a new job with --no-cache, which recomputes and replaces the cached results: %s. Give your operator %s.",
		sc.name, sc.settings.NoCacheCommand, TraceReference(ctx, sc.jobID))}
}

// recordFailed is the error of a stage whose hit entries and stats could
// not be written; sealed says whether its writes were sealed afterwards,
// so that a retry reuses them.
func (e *Engine) recordFailed(ctx context.Context, sc *stageCache, cause error, sealed bool) *StageError {
	e.logger.ErrorContext(ctx, "the stage's result-cache audit entries could not be written, so its results were not saved. "+
		"Check that the database is reachable and that its schema is current (crosscodexd migrate)",
		"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "attempt", sc.attempt, "hits", len(sc.hits),
		"writes_sealed", sealed, "error", cause)
	retry := fmt.Sprintf("Retry this job once the database is reachable: crosscodex run retry %s.", sc.jobID)
	if sealed {
		retry = fmt.Sprintf("The results sent to the model were stored and sealed, so retrying this job reuses them: crosscodex run retry %s.", sc.jobID)
	}
	return &StageError{Analyzer: sc.name, Sentinel: resultcache.ErrUnavailable, Cause: cause, Message: fmt.Sprintf(
		"Stage %q stopped after its LLM calls: its result-cache audit entries could not be written, so its results were not saved "+
			"(error class result_cache_unavailable). %s If this keeps happening, give your operator %s.",
		sc.name, retry, TraceReference(ctx, sc.jobID))}
}

// checkpointFailed is the error of a completed stage that could not be
// sealed. A record mismatch is result_cache_corrupt unless a write's
// outcome was unknown, which may explain it: then it is
// result_cache_unavailable, and the seal is refused either way.
func (e *Engine) checkpointFailed(ctx context.Context, sc *stageCache, cause error) *StageError {
	if mismatch(cause) {
		e.countIntegrityFailures(ctx, sc, 1, mismatchReason(sc))
		e.logger.ErrorContext(ctx, mismatchLog(sc), "analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID,
			"attempt", sc.attempt, "uncertain_write", sc.uncertain, "error", cause)
		if !sc.uncertain {
			return &StageError{Analyzer: sc.name, Sentinel: resultcache.ErrCorrupt, Cause: cause, Message: fmt.Sprintf(
				"Stage %q stopped: the database holds result-cache records for this stage run that the stage did not write, "+
					"so the run was not sealed and its results were not saved (error class result_cache_corrupt). "+
					"Start a new job: %s. Give your operator %s.",
				sc.name, sc.settings.NoCacheCommand, TraceReference(ctx, sc.jobID))}
		}
	} else {
		e.logger.ErrorContext(ctx, "the stage's result-cache record could not be sealed, so its results were not saved. "+
			"Check that the database and attestation storage are reachable and the attestation key pair is valid",
			"analyzer", sc.name, "tenant_id", sc.tenantID, "job_id", sc.jobID, "attempt", sc.attempt, "error", cause)
	}
	return &StageError{Analyzer: sc.name, Sentinel: resultcache.ErrUnavailable, Cause: cause, Message: fmt.Sprintf(
		"Stage %q stopped after its LLM calls: its result-cache record could not be sealed, so its results were not saved "+
			"(error class result_cache_unavailable). Retry this job once the database and attestation storage are reachable "+
			"(crosscodex run retry %s). If this keeps happening, give your operator %s.",
		sc.name, sc.jobID, TraceReference(ctx, sc.jobID))}
}
