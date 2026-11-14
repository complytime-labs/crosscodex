package analysis_test

import (
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

// drawBackoffInputs draws a retry attempt (0..10) and a base delay of
// minBaseMs..5000 milliseconds.
func drawBackoffInputs(t *rapid.T, minBaseMs int) (int, time.Duration) {
	baseMs := rapid.IntRange(minBaseMs, 5000).Draw(t, "baseMs")
	attempt := rapid.IntRange(0, 10).Draw(t, "attempt")
	return attempt, time.Duration(baseMs) * time.Millisecond
}

var _ = Describe("Property Specifications", Ordered, func() {
	Context("computeBackoff — bounded exponential with jitter", func() {
		It("never exceeds base * 2^attempt * 1.25", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				attempt, base := drawBackoffInputs(t, 100)

				result := analysis.ExportComputeBackoff(attempt, base)

				maxExpected := time.Duration(float64(base) * float64(int(1)<<uint(attempt)) * 1.25)
				if result > maxExpected {
					t.Fatalf("backoff %v exceeded max %v for base=%v attempt=%d", result, maxExpected, base, attempt)
				}
			})
		})

		It("never goes below base * 2^attempt * 0.75", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				attempt, base := drawBackoffInputs(t, 100)

				result := analysis.ExportComputeBackoff(attempt, base)

				minExpected := time.Duration(float64(base) * float64(int(1)<<uint(attempt)) * 0.75)
				if result < minExpected {
					t.Fatalf("backoff %v below min %v for base=%v attempt=%d", result, minExpected, base, attempt)
				}
			})
		})

		It("is always non-negative", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				attempt, base := drawBackoffInputs(t, 0)

				result := analysis.ExportComputeBackoff(attempt, base)
				if result < 0 {
					t.Fatalf("negative backoff %v for base=%v attempt=%d", result, base, attempt)
				}
			})
		})
	})

	Context("orderByModel — dispatch order grouped by model", func() {
		It("is a permutation of the tasks, grouped by model in panel order, stable within a model", func() {
			aliases := []string{"panel-1", "panel-2", "panel-3", "other", ""}
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				panel := rapid.SliceOfNDistinct(rapid.SampledFrom(aliases[:4]), 0, 4, rapid.ID[string]).Draw(t, "panel")
				models := rapid.SliceOfN(rapid.SampledFrom(aliases), 0, 40).Draw(t, "models")
				tasks := make([]analyzer.Task, len(models))
				modelOf := make(map[string]string, len(models))
				for i, m := range models {
					fields := map[string]any{}
					if m != "" {
						fields["model"] = m
					}
					payload, err := structpb.NewStruct(fields)
					if err != nil {
						t.Fatalf("building payload: %v", err)
					}
					id := fmt.Sprintf("t%d", i)
					tasks[i] = analyzer.Task{TaskID: id, Payload: payload}
					modelOf[id] = m
				}
				before := slices.Clone(tasks)

				got := analysis.ExportOrderByModel(tasks, panel)

				if !slices.Equal(taskIDsOf(tasks), taskIDsOf(before)) {
					t.Fatalf("input reordered: %v", taskIDsOf(tasks))
				}
				gotIDs := taskIDsOf(got)
				sortedGot, sortedIn := slices.Sorted(slices.Values(gotIDs)), slices.Sorted(slices.Values(taskIDsOf(tasks)))
				if !slices.Equal(sortedGot, sortedIn) {
					t.Fatalf("not a permutation: got %v from %v", gotIDs, taskIDsOf(tasks))
				}
				// Groups: the sequence of models with consecutive repeats
				// collapsed must name each model once.
				var groups []string
				for i, id := range gotIDs {
					if i == 0 || modelOf[id] != modelOf[gotIDs[i-1]] {
						groups = append(groups, modelOf[id])
					}
				}
				seen := map[string]bool{}
				for _, g := range groups {
					if seen[g] {
						t.Fatalf("model %q dispatched in more than one run: %v", g, groups)
					}
					seen[g] = true
				}
				// Panel aliases come first, in panel order.
				var wantPanel []string
				for _, a := range panel {
					if seen[a] {
						wantPanel = append(wantPanel, a)
					}
				}
				if !slices.Equal(groups[:len(wantPanel)], wantPanel) {
					t.Fatalf("groups %v do not start with the panel's order %v", groups, wantPanel)
				}
				// Models outside the panel follow, in first-seen order.
				var wantRest []string
				for _, m := range models {
					if !slices.Contains(panel, m) && !slices.Contains(wantRest, m) {
						wantRest = append(wantRest, m)
					}
				}
				if !slices.Equal(groups[len(wantPanel):], wantRest) {
					t.Fatalf("groups %v after the panel prefix are not the other models in first-seen order %v", groups, wantRest)
				}
				// Stable: within a model, tasks keep their generated order
				// (IDs t<i> were assigned in generation order).
				last := map[string]int{}
				for _, id := range gotIDs {
					var n int
					if _, err := fmt.Sscanf(id, "t%d", &n); err != nil {
						t.Fatalf("parsing %q: %v", id, err)
					}
					if prev, ok := last[modelOf[id]]; ok && n < prev {
						t.Fatalf("model %q reordered: %v", modelOf[id], gotIDs)
					}
					last[modelOf[id]] = n
				}
			})
		})
	})
})

func taskIDsOf(tasks []analyzer.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.TaskID
	}
	return ids
}
