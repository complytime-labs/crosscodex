package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// modelRevisionsOf maps each panel alias to its llm.model_revisions value,
// "" when the deployment pins none. The map is never nil, so a stage without a
// panel records an empty model set.
func (s *Service) modelRevisionsOf(aliases []string) map[string]string {
	models := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		models[alias] = s.llmCfg.ModelRevisions[alias]
	}
	return models
}

// stageRecordFor returns the JSON cleaning.StageRecord stored with the
// completed analyzer stage name: the prompt and prompt version its Aggregate
// reported in output.Metadata, and the revision of each alias of its panel.
// It returns "" when the output names no prompt (a stage that renders none,
// such as embedding), which stores NULL: unknown, never stale. A record that
// cannot be stored (cleaning.ErrInvalidStageRecord) is an error, so the stage
// is never completed with a record a cleaning pass could not read back.
func (s *Service) stageRecordFor(name string, output analyzer.Output) (string, error) {
	promptName := output.Metadata["prompt_name"]
	if promptName == "" {
		return "", nil
	}
	panel, _, err := s.panelSpecOf(name)
	if err != nil {
		return "", err
	}
	record := cleaning.StageRecord{
		PromptName:    promptName,
		PromptVersion: output.Metadata["prompt_version"],
		Models:        s.modelRevisionsOf(panel.Aliases),
	}
	stored, err := record.Marshal()
	if err != nil {
		return "", fmt.Errorf("stage %q: %w", name, err)
	}
	return stored, nil
}

// CurrentStageRecord returns the record the stage would be stored with if it
// ran now: the version of its prompt as tenantID's prompt layers resolve it,
// and the revision currently configured for each alias of its panel. A
// cleaning pass compares it with a stored Stage.StageRecord to find a stage
// produced by a stale prompt or model. It returns false, without error, when
// stage is not a registered analyzer or renders no prompt, so there is nothing
// to compare. It fails closed: with no prompt registry (WithPromptRegistry) or
// an unresolvable prompt it returns an error instead of guessing a version.
func (s *Service) CurrentStageRecord(ctx context.Context, tenantID, stage string) (cleaning.StageRecord, bool, error) {
	ctx, err := tenant.WithTenant(ctx, tenantID)
	if err != nil {
		return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: %w", err)
	}
	if s.registry == nil {
		return cleaning.StageRecord{}, false, nil
	}
	ra, err := s.registry.Get(stage)
	if errors.Is(err, analyzer.ErrNotFound) {
		return cleaning.StageRecord{}, false, nil
	}
	if err != nil {
		return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: analyzer %q: %w", stage, err)
	}
	prompted, ok := analyzer.AsPrompted(ra)
	if !ok {
		return cleaning.StageRecord{}, false, nil
	}
	if s.prompts == nil {
		return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: stage %q renders prompt %q but the pipeline has no prompt registry, "+
			"so its prompt version cannot be read. Build the service with pipeline.WithPromptRegistry", stage, prompted.PromptName())
	}
	spec, err := s.prompts.Resolve(ctx, prompted.PromptName())
	if err != nil {
		return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: resolving prompt %q for stage %q: %w", prompted.PromptName(), stage, err)
	}
	panel, _, err := s.panelSpecOf(stage)
	if err != nil {
		return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: %w", err)
	}
	return cleaning.StageRecord{
		PromptName:    prompted.PromptName(),
		PromptVersion: spec.Version,
		Models:        s.modelRevisionsOf(panel.Aliases),
	}, true, nil
}
