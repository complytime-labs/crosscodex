package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// cacheModesToProto maps a stage's recorded mode to its proto value.
var cacheModesToProto = map[resultcache.Mode]pb.ResultCacheMode{
	resultcache.ModeEnabled:  pb.ResultCacheMode_RESULT_CACHE_MODE_ENABLED,
	resultcache.ModeBypassed: pb.ResultCacheMode_RESULT_CACHE_MODE_BYPASSED,
	resultcache.ModeDisabled: pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED,
	resultcache.ModeSkipped:  pb.ResultCacheMode_RESULT_CACHE_MODE_SKIPPED,
}

// cacheSummaries returns how each analyzer stage of job used the result
// cache, in the order of stages, for GetJob. Only signed records are shown:
//
//   - a completed stage naming the attempt that sealed it
//     (Stage.CacheAttempt, untrusted) shows that attempt's stats, if its
//     checkpoint verifies under the tenant's key and its rows still match
//     the digest the checkpoint signs (resultcache.Store.VerifiedSummaries);
//   - a completed stage naming none kept no cache records: it shows the
//     mode and reason its signed attestation link records, if that mode is
//     one that keeps none (disabled, off or unrecorded) and the link's product is the
//     result stored now; while the job runs and its links are not signed
//     yet, it is in progress;
//   - a stage not completed is in progress while the job runs.
//
// Anything else is unattested, with every field but the analyzer empty,
// including an analyzer whose signed records object storage could not
// return: that failure is logged once and does not fail run status.
// Producing jobs the caller (tenant.UserFromContext) created are named; the
// rest are only counted, because GetJob is owner-only and naming another
// user's job would reveal that it exists. It costs at most one object read
// per analyzer (a checkpoint or a link, never a listing), one
// VerifiedSummaries call, one JobsCreatedBy call and, when a stage kept no
// records, one read of the stored results. Without the result
// cache, or for a tenant whose jobs are not attested (which reuse
// nothing), it returns no summaries.
func (s *Service) cacheSummaries(ctx context.Context, job *Job, stages []*Stage) ([]*pb.AnalyzerCacheSummary, error) {
	if !s.cacheWired || s.cacheStore == nil {
		return nil, nil
	}
	attestor := s.attestors.forTenant(job.TenantID)
	if attestor == nil {
		return nil, nil
	}
	var analyzers []*Stage
	var names []string
	for _, st := range stages {
		if IsAnalyzerStage(st.StageName) {
			analyzers = append(analyzers, st)
			names = append(names, st.StageName)
		}
	}
	if len(analyzers) == 0 {
		return nil, nil
	}
	verifier := s.newCacheCheckpoints(job, attestor, names)
	running := job.Status == JobStatusRunning || job.Status == JobStatusPending

	var unsealed []string
	for _, st := range analyzers {
		if st.Status == StageStatusCompleted && st.CacheAttempt == "" {
			unsealed = append(unsealed, st.StageName)
		}
	}
	// A link vouches only for the result it was signed over: the stored
	// results of the stages that kept no records, read once.
	var results map[string]*analyzer.Output
	if len(unsealed) > 0 {
		var err error
		if results, err = s.store.GetCompletedAnalysisResults(ctx, job.JobID, unsealed); err != nil {
			return nil, fmt.Errorf("reading the stored results of job %s: %w", job.JobID, err)
		}
	}

	out := make([]*pb.AnalyzerCacheSummary, len(analyzers))
	byName := make(map[string]*pb.AnalyzerCacheSummary, len(analyzers))
	signed := map[string]resultcache.SignedAttempt{}
	// Object storage failing does not fail run status: the analyzers whose
	// signed records cannot be read show as unattested, and the cause is
	// logged once.
	var unreadable []string
	var unreadableErr error
	for i, st := range analyzers {
		sum := &pb.AnalyzerCacheSummary{Analyzer: st.StageName,
			Attestation: pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_UNATTESTED}
		out[i] = sum
		byName[st.StageName] = sum
		switch {
		case st.Status != StageStatusCompleted:
			if running {
				sum.Attestation = pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS
			}
		case st.CacheAttempt != "":
			digest, ok, err := s.summaryCheckpoint(ctx, verifier, job, st)
			if err != nil {
				unreadable = append(unreadable, st.StageName)
				unreadableErr = cmp.Or(unreadableErr, err)
				continue
			}
			if ok {
				signed[st.StageName] = resultcache.SignedAttempt{Attempt: st.CacheAttempt, Digest: digest}
			}
		default:
			var stored []byte
			if res, ok := results[st.StageName]; ok {
				stored = res.ResultData
			}
			mode, reason, found, ok, err := s.summaryLinkMode(ctx, job, attestor, st.StageName, stored)
			switch {
			case err != nil:
				unreadable = append(unreadable, st.StageName)
				unreadableErr = cmp.Or(unreadableErr, err)
			case ok:
				sum.Mode = mode
				sum.Reason = reason
				sum.Attestation = pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED
			case !found && running:
				// The job's links are signed once its analysis ends.
				sum.Attestation = pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS
			}
		}
	}
	if len(unreadable) > 0 {
		s.logger.WarnContext(ctx, "object storage could not be read, so run status shows these analyzers as unattested. "+
			"Check that object storage (storage.objects.base_path) is readable",
			"job_id", job.JobID, "analyzers", unreadable, "trace_id", telemetry.TraceIDFromContext(ctx), "error", unreadableErr)
	}
	if len(signed) == 0 {
		return out, nil
	}

	verified, err := s.cacheStore.VerifiedSummaries(ctx, job.JobID, signed)
	if err != nil {
		return nil, fmt.Errorf("reading the result-cache records of job %s: %w", job.JobID, err)
	}
	producers := map[string]bool{}
	var attested []resultcache.Summary
	for _, v := range verified {
		want, ok := signed[v.Analyzer]
		// An attempt that did not complete the stage recorded no stats: the
		// stage's pointer was changed, so it vouches for nothing.
		if !ok || v.Attempt != want.Attempt || !v.Attested || v.Stats.Mode == "" {
			continue
		}
		attested = append(attested, v)
		for _, p := range v.ProducerJobIDs {
			producers[p] = true
		}
	}
	var owned []string
	if len(producers) > 0 {
		if owned, err = s.store.JobsCreatedBy(ctx, slices.Sorted(maps.Keys(producers)), tenant.UserFromContext(ctx)); err != nil {
			return nil, fmt.Errorf("reading which reused jobs the caller created: %w", err)
		}
	}
	for _, v := range attested {
		sum := byName[v.Analyzer]
		sum.Attestation = pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED
		sum.Mode = cacheModesToProto[v.Stats.Mode]
		sum.Reason = v.Stats.Reason
		sum.Tasks, sum.Hits, sum.Dispatched = int64(v.Stats.Tasks), int64(v.Stats.Hits), int64(v.Stats.Dispatched)
		for _, alias := range slices.Sorted(maps.Keys(v.Stats.Models)) {
			m := v.Stats.Models[alias]
			sum.Models = append(sum.Models, &pb.AnalyzerCacheModel{Alias: alias, ObservedModel: m.Observed, Revision: m.Revision})
		}
		for _, p := range v.ProducerJobIDs {
			if slices.Contains(owned, p) {
				sum.ProducerJobIds = append(sum.ProducerJobIds, p)
			} else {
				sum.OtherProducerJobs++
			}
		}
	}
	return out, nil
}

// summaryCheckpoint verifies the checkpoint of the attempt that sealed st
// and returns the digest it signs; ok is false when it does not verify,
// which is logged and shown as unattested. Only a failure to read it from
// object storage is an error.
func (s *Service) summaryCheckpoint(ctx context.Context, verifier *cacheCheckpoints, job *Job, st *Stage) (string, bool, error) {
	vctx, finish := s.startCheckpointOp(ctx, "verify", job.JobID, st.StageName)
	digest, err := verifier.ownCheckpoint(vctx, st.StageName, st.CacheAttempt)
	finish(err)
	switch {
	case err == nil:
		return digest, true, nil
	case errors.Is(err, errCheckpointUnreadable):
		return "", false, fmt.Errorf("reading the result-cache checkpoint of job %s analyzer %q: %w", job.JobID, st.StageName, err)
	case errors.Is(err, resultcache.ErrCorrupt):
		s.logger.ErrorContext(ctx, "a result-cache checkpoint failed its integrity check, so run status shows the analyzer as "+
			"unattested. The checkpoint was replaced after signing; check who can write to object storage",
			"job_id", job.JobID, "analyzer", st.StageName, "attempt", st.CacheAttempt,
			"trace_id", telemetry.TraceIDFromContext(ctx), "error", err)
	default:
		s.logger.WarnContext(ctx, "the result-cache checkpoint of a completed stage does not verify, so run status shows the "+
			"analyzer as unattested: it is missing, signed by a key no longer trusted, or the stage names no valid attempt",
			"job_id", job.JobID, "analyzer", st.StageName, "attempt", st.CacheAttempt,
			"trace_id", telemetry.TraceIDFromContext(ctx), "error", err)
	}
	return "", false, nil
}

// summaryLinkMode reads analyzer name's signed attestation link and returns
// the mode and reason it records when its result_cache.mode is one that
// keeps no cache records and its <name>.output product is the content
// digest of stored, the analyzer's result stored now. disabled and off
// show as DISABLED; unrecorded (a stage an earlier run completed without
// naming its attempt) as UNRECORDED, never as disabled, since its mode is
// not known. found is false when there is no link. ok is false for a
// missing link, one that does not verify for this job and analyzer, one
// over another result, or another mode. Only a failure to read it from
// object storage is an error.
func (s *Service) summaryLinkMode(ctx context.Context, job *Job, attestor *Attestor, name string, stored []byte) (mode pb.ResultCacheMode, reason string, found, ok bool, err error) {
	key := attestationKey(job, "links/"+name+".json")
	raw, err := s.readAttestation(ctx, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return 0, "", false, false, nil
	case errors.Is(err, errAttestationTooLarge):
		s.logger.ErrorContext(ctx, "an attestation link is larger than the limit, so run status shows the analyzer as unattested. "+
			"The link was replaced after signing; check who can write to object storage",
			"job_id", job.JobID, "analyzer", name, "key", key, "trace_id", telemetry.TraceIDFromContext(ctx))
		return 0, "", true, false, nil
	case err != nil:
		return 0, "", true, false, fmt.Errorf("reading the attestation link of job %s analyzer %q: %w", job.JobID, name, err)
	}
	verified, err := attestor.Generator.Verify(ctx, raw)
	if err == nil && (verified.Step != name || !slices.Contains(verified.Materials, jobSubject(job))) {
		err = fmt.Errorf("it is signed for step %q or another job", verified.Step)
	}
	if err == nil && (stored == nil || !slices.Contains(verified.Products,
		attestation.Artifact{URI: name + ".output", Digest: contentDigest(stored)})) {
		err = errors.New("it is signed over another result than the one stored now")
	}
	if err != nil {
		s.logger.WarnContext(ctx, "an analyzer's attestation link does not verify, so run status shows the analyzer as unattested",
			"job_id", job.JobID, "analyzer", name, "key", key, "trace_id", telemetry.TraceIDFromContext(ctx), "error", err)
		return 0, "", true, false, nil
	}
	signedMode, _ := verified.ByProducts[byproductCacheMode].(string)
	switch signedMode {
	case string(resultcache.ModeDisabled), cacheModeOff:
		mode = pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED
	case cacheModeUnrecorded:
		mode = pb.ResultCacheMode_RESULT_CACHE_MODE_UNRECORDED
	default:
		return 0, "", true, false, nil
	}
	reason, _ = verified.ByProducts[byproductCacheReason].(string)
	return mode, reason, true, true, nil
}
