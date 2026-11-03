package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// The byproducts an analyzer's attestation link records about its
// result-cache use (#243). Byproducts are inside the signed link, so they
// prove the mode that ran even when the cache store is not used.
const (
	byproductCacheMode    = "result_cache.mode"
	byproductCacheReason  = "result_cache.reason"
	byproductCacheAttempt = "result_cache.attempt"
)

// Link modes beyond resultcache.Mode: off is a run without the result cache
// wired; unrecorded is a stage an earlier run completed without naming the
// attempt that sealed it, whose mode is never inferred.
const (
	cacheModeOff        = "off"
	cacheModeUnrecorded = "unrecorded"
)

// noCacheRecordDigest is the cache_manifest material of a stage no
// checkpoint sealed: the digest of the empty manifest, a fixed value.
var noCacheRecordDigest = resultcache.Manifest{}.Digest()

// cacheManifestURI is the URI of an analyzer link's result-cache material.
func cacheManifestURI(analyzerName string) string {
	return analyzerName + ".cache_manifest"
}

// analyzerCacheRun is an analyzer's result-cache use in the run that
// completed it: its effective stats (hasStats false when the run had no
// cache mode) and the attempt that sealed it ("" when none did).
type analyzerCacheRun struct {
	stats    resultcache.Stats
	hasStats bool
	attempt  string
}

// analyzerCacheRecord is what an analyzer's link records about its cache
// use: the checkpoint digest it pins and the signed byproducts.
type analyzerCacheRecord struct {
	digest  string
	mode    string
	reason  string
	attempt string
}

func (r analyzerCacheRecord) byProducts() map[string]any {
	return map[string]any{byproductCacheMode: r.mode, byproductCacheReason: r.reason, byproductCacheAttempt: r.attempt}
}

// cacheRecordOf returns analyzer name's result-cache record for its link.
//
// For a stage this run completed it is the run's own record: the digest of
// the checkpoint this run signed and stored for the sealing attempt, and the
// engine's effective mode. For a stage an earlier run completed it comes
// from the attempt stage.CacheAttempt names. That pointer is not trusted:
// its checkpoint must verify under the tenant's key, and the attempt's rows
// must still match the digest it signs (VerifiedSummaries recomputes it).
// A database writer can only repoint it to another signed attempt of the
// same job and analyzer that completed the stage, which still pins genuine
// records. A null pointer is recorded as unrecorded, never inferred.
func (s *Service) cacheRecordOf(ctx context.Context, job *Job, exec *jobExecution, name string, stage *Stage) (analyzerCacheRecord, error) {
	if run, ok := exec.cacheRuns[name]; ok {
		if run.attempt == "" {
			if !run.hasStats {
				return analyzerCacheRecord{digest: noCacheRecordDigest, mode: cacheModeOff,
					reason: "the result cache is not wired on this daemon"}, nil
			}
			return analyzerCacheRecord{digest: noCacheRecordDigest, mode: string(run.stats.Mode), reason: run.stats.Reason}, nil
		}
		ref := resultcache.AttemptRef{JobID: job.JobID, Analyzer: name, Attempt: run.attempt}
		var digest string
		stored := false
		if exec.checkpoints != nil {
			digest, stored = exec.checkpoints.storedDigest(ref)
		}
		if !stored {
			return analyzerCacheRecord{}, fmt.Errorf("job %s: analyzer %q completed, sealed by result-cache attempt %s, "+
				"but this run stored no checkpoint for that attempt, so its link cannot pin it. %s",
				job.JobID, name, run.attempt, retryHint(job))
		}
		return analyzerCacheRecord{digest: digest, mode: string(run.stats.Mode), reason: run.stats.Reason, attempt: run.attempt}, nil
	}

	attempt := ""
	if stage != nil {
		attempt = stage.CacheAttempt
	}
	verifier := exec.checkpoints
	if verifier == nil {
		verifier = s.newCacheCheckpoints(job, exec.attestor, exec.dag.Order())
	}
	if attempt == "" {
		if err := s.checkNoSignedCompletion(ctx, job, verifier, name); err != nil {
			return analyzerCacheRecord{}, err
		}
		return analyzerCacheRecord{digest: noCacheRecordDigest, mode: cacheModeUnrecorded,
			reason: "the stage completed in an earlier run that recorded no result-cache attempt: " +
				"the cache was disabled or off then"}, nil
	}
	invalid := func(why string, cause error) error {
		return fmt.Errorf("job %s: analyzer %q completed in an earlier run, sealed by result-cache attempt %q, but %s: %w: %w. "+
			"Its cache use cannot be attested; start a new job: %s",
			job.JobID, name, attempt, why, cause, ErrAttestationInvalid, newJobCommand(job))
	}
	digest, err := verifier.ownCheckpoint(ctx, name, attempt)
	if err != nil {
		if untrusted, ok := errors.AsType[*attestation.UntrustedKeyError](err); ok {
			return analyzerCacheRecord{}, invalid(fmt.Sprintf("its checkpoint is signed by a key that is not trusted now "+
				"(key IDs %s): the attestation key pair changed since that run", strings.Join(untrusted.KeyIDs, ", ")), err)
		}
		switch {
		case errors.Is(err, errInvalidAttempt):
			return analyzerCacheRecord{}, invalid("that attempt is not a canonical UUID, so the stage's record was changed", err)
		case errors.Is(err, storage.ErrNotFound):
			return analyzerCacheRecord{}, invalid("that attempt has no stored checkpoint", err)
		case errors.Is(err, errCheckpointUnreadable):
			return analyzerCacheRecord{}, fmt.Errorf("job %s: the result-cache checkpoint of analyzer %q could not be read: %w: %w. "+
				"Check that object storage (storage.objects.base_path) is readable. %s",
				job.JobID, name, err, ErrAttestationStorage, retryHint(job))
		default:
			return analyzerCacheRecord{}, invalid("its checkpoint does not verify", err)
		}
	}
	if s.cacheStore == nil {
		return analyzerCacheRecord{}, cacheNotWired(job, name)
	}
	sums, err := s.cacheStore.VerifiedSummaries(ctx, job.JobID, map[string]resultcache.SignedAttempt{name: {Attempt: attempt, Digest: digest}})
	if err != nil {
		return analyzerCacheRecord{}, cacheRecordUnreadable(job, name, err)
	}
	for _, sum := range sums {
		if sum.Analyzer != name || sum.Attempt != attempt {
			continue
		}
		switch {
		case !sum.Attested:
			return analyzerCacheRecord{}, invalid("the attempt's records changed after it was signed", errAttemptRecordChanged)
		case sum.Stats.Mode == "":
			return analyzerCacheRecord{}, invalid("that attempt did not complete the stage (its record has no stage statistics)",
				errAttemptRecordChanged)
		}
		return analyzerCacheRecord{digest: digest, mode: string(sum.Stats.Mode), reason: sum.Stats.Reason, attempt: attempt}, nil
	}
	return analyzerCacheRecord{}, fmt.Errorf("job %s: verifying the result-cache record of analyzer %q returned no summary for attempt %s. %s",
		job.JobID, name, attempt, retryHint(job))
}

// errAttemptRecordChanged reports a sealing attempt whose rows no longer
// match its checkpoint, or that never completed the stage.
var errAttemptRecordChanged = errors.New("the result-cache record does not match its checkpoint")

// cacheNotWired is the error of a stage an earlier run completed with the
// result cache, which this daemon cannot verify without it.
func cacheNotWired(job *Job, name string) error {
	return fmt.Errorf("job %s: analyzer %q completed in an earlier run with the result cache, but this daemon has no "+
		"result cache wired to verify that record. To resume it, configure analysis.result_cache and restart crosscodexd. %s",
		job.JobID, name, retryHint(job))
}

// cacheRecordUnreadable is the error of a stage whose result-cache rows
// could not be read to verify them against its checkpoint.
func cacheRecordUnreadable(job *Job, name string, err error) error {
	return fmt.Errorf("job %s: the result-cache record of analyzer %q could not be read to verify it: %w: %w. "+
		"Check that the database is reachable. %s", job.JobID, name, err, ErrAttestationStorage, retryHint(job))
}

// checkNoSignedCompletion fails, as ErrAttestationInvalid, a stage an
// earlier run completed that names no sealing attempt although one of its
// job's checkpoints for the analyzer verifies and seals a completed stage
// run (its record has stage statistics): the pointer was removed, and
// signing the stage as unrecorded would hide that record. It reads the
// analyzer's checkpoints once (one listing) and each verified attempt's
// rows; it detects only a completion whose checkpoint is still stored and
// signed by a key trusted now.
func (s *Service) checkNoSignedCompletion(ctx context.Context, job *Job, verifier *cacheCheckpoints, name string) error {
	vc, err := verifier.Verified(ctx, name, []string{job.JobID})
	switch {
	case errors.Is(err, resultcache.ErrCorrupt):
		return fmt.Errorf("job %s: analyzer %q completed in an earlier run that recorded no result-cache attempt, and one of "+
			"its checkpoints does not verify: %w: %w. Its cache use cannot be attested; start a new job: %s",
			job.JobID, name, err, ErrAttestationInvalid, newJobCommand(job))
	case err != nil:
		return fmt.Errorf("job %s: the result-cache checkpoints of analyzer %q could not be read: %w: %w. "+
			"Check that object storage (storage.objects.base_path) is readable. %s",
			job.JobID, name, err, ErrAttestationStorage, retryHint(job))
	}
	if len(vc.Signed) == 0 {
		return nil
	}
	if s.cacheStore == nil {
		return cacheNotWired(job, name)
	}
	refs := slices.SortedFunc(maps.Keys(vc.Signed), func(a, b resultcache.AttemptRef) int { return strings.Compare(a.Attempt, b.Attempt) })
	for _, ref := range refs {
		sums, err := s.cacheStore.VerifiedSummaries(ctx, job.JobID,
			map[string]resultcache.SignedAttempt{name: {Attempt: ref.Attempt, Digest: vc.Signed[ref]}})
		if err != nil {
			return cacheRecordUnreadable(job, name, err)
		}
		for _, sum := range sums {
			if sum.Attempt == ref.Attempt && sum.Attested && sum.Stats.Mode != "" {
				return fmt.Errorf("job %s: analyzer %q completed in an earlier run, but the stage's result-cache record pointer "+
					"is missing although a signed record of its completion exists (attempt %s): %w. The stage's record was "+
					"changed, so its cache use cannot be attested; start a new job: %s",
					job.JobID, name, ref.Attempt, ErrAttestationInvalid, newJobCommand(job))
			}
		}
	}
	return nil
}
