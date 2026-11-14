package pipeline

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// candidateDependentAnalyzers run in the pipeline's second pass, after
// candidate generation, with the pair parameters (every control's text) and
// no per-control input. requires and relationship need candidate data
// (requires_candidates / relationship_candidates) to exist before
// GenerateWork runs. critique reviews their results, so it needs the same
// parameters and must run in their pass: in the first pass the engine would
// skip it, its dependencies not having run yet.
var candidateDependentAnalyzers = map[string]bool{"requires": true, "relationship": true, "critique": true}

// buildStageNames returns a job's stages: the registered analyzers in
// dagOrder, with a candidate_generation stage inserted before the first
// candidate-dependent one, then the fixed synthesis/graph stages. Without a
// candidate-dependent analyzer (both pair analyzers disabled) nothing would
// read candidates, so there is no candidate_generation stage. dagOrder is
// already topologically sorted, so the DAG's own dependency edges
// (requires/relationship depend on embedding) guarantee everything before the
// first candidate-dependent name has already run by the time candidate
// generation needs it.
func buildStageNames(dagOrder []string) []string {
	stageNames := make([]string, 0, len(dagOrder)+3)
	inserted := false
	for _, name := range dagOrder {
		if candidateDependentAnalyzers[name] && !inserted {
			stageNames = append(stageNames, "candidate_generation")
			inserted = true
		}
		stageNames = append(stageNames, name)
	}
	return append(stageNames, "synthesis", "graph")
}

// pipelineStages are the stages buildStageNames adds that are not analyzers.
var pipelineStages = map[string]bool{"candidate_generation": true, "synthesis": true, "graph": true}

// IsAnalyzerStage reports whether a job stage named name runs an analyzer,
// as opposed to one of the stages the pipeline adds (candidate_generation,
// synthesis, graph).
func IsAnalyzerStage(name string) bool { return !pipelineStages[name] }

// checkJobAnalyzers returns an error wrapping ErrAnalyzersChanged when the
// analyzer stages job was created with differ from dagOrder, the analyzers
// this daemon registers (its enabled ones). A job runs the analyzers it was
// created with: a stage whose analyzer is no longer registered would never
// run, and an analyzer with no stage row would run, bill LLM calls and lose
// its result, so neither is skipped silently.
func checkJobAnalyzers(job *Job, stages []*Stage, dagOrder []string) error {
	created := make(map[string]bool, len(stages))
	for _, st := range stages {
		if IsAnalyzerStage(st.StageName) {
			created[st.StageName] = true
		}
	}
	running := make(map[string]bool, len(dagOrder))
	for _, name := range dagOrder {
		running[name] = true
	}

	var changes, restore []string
	for _, name := range slices.Sorted(maps.Keys(created)) {
		if !running[name] {
			setting := analyzerSetting(name)
			changes = append(changes, fmt.Sprintf("analyzer %q ran when the job was created, but %s is now false", name, setting))
			restore = append(restore, setting+" to true")
		}
	}
	for _, name := range dagOrder {
		if !created[name] {
			setting := analyzerSetting(name)
			changes = append(changes, fmt.Sprintf("analyzer %q did not run when the job was created, but %s is now true", name, setting))
			restore = append(restore, setting+" to false")
		}
	}
	if len(changes) == 0 {
		return nil
	}

	// The job is failed, and a failed job is never resumed, so a new job
	// is the only remedy; the settings only choose its analyzers.
	return &tenantError{sentinel: ErrAnalyzersChanged, message: fmt.Sprintf("job %s cannot run: %s: %v. "+
		"The job is marked failed and is not resumed. Start a new job with the analyzers enabled now: %s. "+
		"To give the new job the analyzers this one was created with, first set %s",
		job.JobID, strings.Join(changes, "; "), ErrAnalyzersChanged, newJobCommand(job), strings.Join(restore, " and "))}
}

// analyzerSetting names the setting that enables analyzer name, or says that
// no setting does for an analyzer outside the production set.
func analyzerSetting(name string) string {
	if setting, ok := config.AnalyzerSetting(name, "enabled"); ok {
		return setting
	}
	return fmt.Sprintf("the setting enabling %q", name)
}
