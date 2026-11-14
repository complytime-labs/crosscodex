package critique_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	"google.golang.org/protobuf/types/known/structpb"
	"pgregory.net/rapid"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Suite bootstrap lives in critique_bdd_test.go.

var _ = Describe("Property Specifications", Ordered, func() {
	responses := []string{
		"VERDICT: UPHOLD\nREASON: same duty",
		"VERDICT: REJECT\nREASON: topic only",
		"VERDICT: MAYBE\nREASON: unsure",
		"",
	}

	It("counts every panel vote once per pair, and vetoes only with a REJECT vote", func() {
		ctx := testspecs.SetupTenantContext("test-tenant")
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			models := rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"panel-1", "panel-2", "panel-3", "panel-4", "panel-5"}), 1, 5,
				rapid.ID[string]).Draw(t, "models")
			cfg := config.CritiqueConfig{
				Models: models, SamplesPerModel: rapid.IntRange(1, 3).Draw(t, "samples"),
				ConsensusThreshold: rapid.Float64Range(0.5, 1.0).Draw(t, "threshold"),
				Veto:               rapid.Bool().Draw(t, "veto"),
				MaxErrorRate:       rapid.Float64Range(0, 1).Draw(t, "max_error_rate"),
			}
			a := critique.New(nil, nil, nil, cfg)
			panel := len(models) * cfg.SamplesPerModel

			pairs := rapid.IntRange(1, 4).Draw(t, "pairs")
			var trs []analyzer.TaskResult
			for p := range pairs {
				kind := rapid.SampledFrom([]string{critique.KindRelationship, critique.KindRequires}).Draw(t, "kind")
				for _, m := range models {
					for s := range cfg.SamplesPerModel {
						if rapid.Bool().Draw(t, "dropped") {
							continue // a vote with no result
						}
						id := fmt.Sprintf("critique-%s-AC-%d--AC-%d-%s-s%d", kind, p, p+1, m, s)
						res, err := structpb.NewStruct(map[string]interface{}{"response": rapid.SampledFrom(responses).Draw(t, "response")})
						if err != nil {
							t.Fatalf("building a result: %v", err)
						}
						tr := analyzer.TaskResult{TaskID: id, TaskType: "critique", Result: res}
						if rapid.IntRange(0, 9).Draw(t, "error") == 0 {
							tr.Error = fmt.Errorf("task failed")
						}
						trs = append(trs, tr)
					}
				}
			}

			out, err := a.Aggregate(ctx, trs)
			if err != nil {
				t.Fatalf("Aggregate: %v", err)
			}
			var got []results.CritiqueResult
			if err := json.Unmarshal(out.ResultData, &got); err != nil {
				t.Fatalf("decoding the result: %v", err)
			}
			for _, r := range got {
				if r.Upheld+r.Rejected+r.Failed != panel {
					t.Fatalf("%s %s--%s: %d upheld + %d rejected + %d failed != panel size %d",
						r.Kind, r.SourceControlID, r.TargetControlID, r.Upheld, r.Rejected, r.Failed, panel)
				}
				if len(r.Reasons) != panel {
					t.Fatalf("%s %s--%s: %d reasons for a panel of %d", r.Kind, r.SourceControlID, r.TargetControlID, len(r.Reasons), panel)
				}
				switch r.Outcome {
				case results.CritiqueUpheld, results.CritiqueSplit, results.CritiqueRejected, results.CritiqueVetoed, results.CritiqueFailed:
				default:
					t.Fatalf("%s %s--%s: unknown outcome %q", r.Kind, r.SourceControlID, r.TargetControlID, r.Outcome)
				}
				if r.Outcome == results.CritiqueFailed && r.Vetoed {
					t.Fatalf("%s %s--%s: a failed critique vetoed its verdict", r.Kind, r.SourceControlID, r.TargetControlID)
				}
				if r.Vetoed != (r.Outcome == results.CritiqueVetoed) {
					t.Fatalf("%s %s--%s: vetoed %v with outcome %q", r.Kind, r.SourceControlID, r.TargetControlID, r.Vetoed, r.Outcome)
				}
				if r.Outcome == results.CritiqueSplit && (r.Vetoed || r.Rejected == 0) {
					t.Fatalf("%s %s--%s: split without a kept REJECT vote", r.Kind, r.SourceControlID, r.TargetControlID)
				}
				if r.Outcome == results.CritiqueUpheld && r.Rejected != 0 {
					t.Fatalf("%s %s--%s: upheld with %d REJECT votes", r.Kind, r.SourceControlID, r.TargetControlID, r.Rejected)
				}
				if r.Vetoed && r.Rejected == 0 {
					t.Fatalf("%s %s--%s vetoed without a REJECT vote", r.Kind, r.SourceControlID, r.TargetControlID)
				}
				if r.Vetoed && float64(r.Rejected)/float64(r.Upheld+r.Rejected) < cfg.ConsensusThreshold {
					t.Fatalf("%s %s--%s vetoed below the threshold", r.Kind, r.SourceControlID, r.TargetControlID)
				}
				if r.Vetoed && !cfg.Veto {
					t.Fatalf("%s %s--%s vetoed with analysis.critique.veto off", r.Kind, r.SourceControlID, r.TargetControlID)
				}
				reached := r.Upheld+r.Rejected > 0 && float64(r.Rejected)/float64(r.Upheld+r.Rejected) >= cfg.ConsensusThreshold
				if reached && !cfg.Veto && r.Outcome != results.CritiqueRejected && r.Outcome != results.CritiqueFailed {
					t.Fatalf("%s %s--%s: %q at %d of %d valid votes with veto off, want rejected", r.Kind, r.SourceControlID, r.TargetControlID,
						r.Outcome, r.Rejected, r.Upheld+r.Rejected)
				}
				if r.Outcome == results.CritiqueRejected && (cfg.Veto ||
					float64(r.Rejected)/float64(r.Upheld+r.Rejected) < cfg.ConsensusThreshold) {
					t.Fatalf("%s %s--%s rejected with veto %v at %d of %d valid votes", r.Kind, r.SourceControlID, r.TargetControlID,
						cfg.Veto, r.Rejected, r.Upheld+r.Rejected)
				}
			}
		})
	})

	It("keeps every parsed reason storable: bounded, valid UTF-8 and free of NUL", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			reason := rapid.String().Draw(t, "reason")
			c := critique.ParseCritique("VERDICT: REJECT\nREASON: " + reason)
			if len(c.Reason) > intanalyzer.MaxStoredJustificationBytes {
				t.Fatalf("reason of %d bytes exceeds %d", len(c.Reason), intanalyzer.MaxStoredJustificationBytes)
			}
			if !utf8.ValidString(c.Reason) || strings.ContainsRune(c.Reason, 0) {
				t.Fatalf("reason %q is not storable in jsonb", c.Reason)
			}
		})
	})

	It("never reaches the verdict provider without a tenant", func() {
		provider := &fakeVerdicts{verdicts: []critique.ProposedVerdict{{Kind: critique.KindRequires, SourceControlID: "a", TargetControlID: "b"}}}
		a := critique.New(nil, nil, provider, config.CritiqueConfig{Models: []string{"panel-1"}, SamplesPerModel: 1})
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			jobID := rapid.StringN(1, 20, -1).Draw(t, "job_id")
			if _, err := a.GenerateWork(context.Background(), nil, analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": jobID}}); err == nil {
				t.Fatalf("GenerateWork without a tenant succeeded")
			}
			if provider.jobID != "" {
				t.Fatalf("the provider was asked for job %q without a tenant", provider.jobID)
			}
		})
	})
})
