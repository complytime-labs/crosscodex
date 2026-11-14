package pipeline

import (
	"context"
	"errors"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

// CacheCheckpointsFor bridges newCacheCheckpoints to the external specs:
// the result-cache checkpoints of job for the analyzers named, signed and
// verified with attestor.
func (s *Service) CacheCheckpointsFor(job *Job, attestor *Attestor, analyzers ...string) analysis.CacheCheckpoints {
	return s.newCacheCheckpoints(job, attestor, analyzers)
}

// CheckpointKey exposes checkpointKey, where a checkpoint is stored.
var CheckpointKey = checkpointKey

// CheckpointURI exposes checkpointURI, the product a checkpoint signs.
var CheckpointURI = checkpointURI

// CacheCheckpointStep exposes cacheCheckpointStep.
const CacheCheckpointStep = cacheCheckpointStep

// PrepareResultCache bridges prepareResultCache to the external specs for
// job, attested by attestor (nil when its tenant does not attest), over the
// analyzers in names and the job's controls.
func (s *Service) PrepareResultCache(ctx context.Context, job *Job, attestor *Attestor, names []string,
	controls []*pb.Control) (analysis.CacheSettings, error) {
	cats, ok := jobconfig.ExtractCatalogs(job.Config)
	if !ok {
		return analysis.CacheSettings{}, errors.New("export_test: the job's config names no catalog")
	}
	dag, err := s.registry.BuildDAG(ctx)
	if err != nil {
		return analysis.CacheSettings{}, err
	}
	return s.prepareResultCache(ctx, job, cats, &jobExecution{attestor: attestor, subject: jobSubject(job), dag: dag}, names, controls)
}

// MaxAttestationObjectBytes exposes maxAttestationObjectBytes, the largest
// attestation object read back from storage.
const MaxAttestationObjectBytes = maxAttestationObjectBytes

// FailJob bridges failJob to the external specs: it records err as jobID's
// failure, as the executor does when a phase of the job fails; job is nil
// when the executor could not load it.
func (s *Service) FailJob(ctx context.Context, tenantID, jobID string, job *Job, err error) {
	s.failJob(ctx, tenantID, jobID, job, err)
}

// NewTenantError exposes tenantError, a failure whose text is message.
func NewTenantError(sentinel error, message string) error {
	return &tenantError{sentinel: sentinel, message: message}
}

// CheckPanelDiversity bridges checkPanelDiversity to the external specs for
// stage's panel of aliases; it returns the upstream model of each alias.
func (s *Service) CheckPanelDiversity(ctx context.Context, tenantID, jobID, stage string, aliases []string, allowShared bool) (map[string]string, error) {
	ids, err := s.checkPanelDiversity(ctx, tenantID, jobID, panelSpec{Stage: stage, Aliases: aliases, AllowShared: allowShared})
	return modelsOf(ids), err
}

// PanelIdentity is what the check found for one alias.
type PanelIdentity = panelIdentity

// CheckPanelIdentities is CheckPanelDiversity returning everything found per alias.
func (s *Service) CheckPanelIdentities(ctx context.Context, tenantID, jobID, stage string, aliases []string, allowShared bool) (map[string]PanelIdentity, error) {
	return s.checkPanelDiversity(ctx, tenantID, jobID, panelSpec{Stage: stage, Aliases: aliases, AllowShared: allowShared})
}

// PanelByProducts exposes the byproducts a panel analyzer's link records;
// ids is nil for a panel that was not checked.
func PanelByProducts(aliases []string, allowShared bool, ids map[string]PanelIdentity) map[string]any {
	return panelRecordOf(panelSpec{Aliases: aliases, AllowShared: allowShared}, ids).byProducts()
}

// PanelProvenance bridges panelProvenance to the external specs.
func (s *Service) PanelProvenance(stages []*Stage) map[string]*pb.PanelProvenance {
	return s.panelProvenance(stages)
}
