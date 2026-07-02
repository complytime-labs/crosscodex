package analyzer

import (
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

// CountResults tallies completed task results into processed, skipped, and
// error counts. A result is counted as an error if it has a non-nil Error, if
// its Result is not an *pb.AnalysisResult, or if the Result is nil. A result
// is skipped when its Attributes["skipped"] == "true". All other results are
// counted as processed.
//
// This replaces near-identical counting loops in the classify and embedding
// analyzer Aggregate methods. The relationship analyzer uses a different
// counting pattern (total + errors only) and does not use this function.
func CountResults(results []analyzer.TaskResult) (processed, skipped, errors int) {
	for _, r := range results {
		if r.Error != nil {
			errors++
			continue
		}
		ar, ok := r.Result.(*pb.AnalysisResult)
		if !ok {
			errors++
			continue
		}
		if ar.Attributes["skipped"] == "true" {
			skipped++
		} else {
			processed++
		}
	}
	return processed, skipped, errors
}
