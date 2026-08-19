package pipeline

// candidateDependentAnalyzers require candidate data (requires_candidates /
// relationship_candidates) to exist before GenerateWork runs.
var candidateDependentAnalyzers = map[string]bool{"requires": true, "relationship": true}

// buildStageNames inserts a candidate_generation stage between the
// pre-candidate analyzers and the candidate-dependent ones, then appends the
// fixed synthesis/graph stages. dagOrder is already topologically sorted, so
// the DAG's own dependency edges (requires/relationship depend on embedding)
// guarantee everything before the first candidate-dependent name has already
// run by the time candidate generation needs it.
func buildStageNames(dagOrder []string) []string {
	splitAt := len(dagOrder)
	for i, name := range dagOrder {
		if candidateDependentAnalyzers[name] {
			splitAt = i
			break
		}
	}

	stageNames := make([]string, 0, len(dagOrder)+3)
	stageNames = append(stageNames, dagOrder[:splitAt]...)
	stageNames = append(stageNames, "candidate_generation")
	stageNames = append(stageNames, dagOrder[splitAt:]...)
	stageNames = append(stageNames, "synthesis", "graph")
	return stageNames
}
