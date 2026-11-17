package cleaningtest_test

import (
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var (
	propSignals  = []cleaning.Signal{cleaning.SignalLowAgreement, cleaning.SignalStalePrompt, cleaning.SignalConflictsWith}
	propStatuses = []cleaning.Status{cleaning.StatusReported, cleaning.StatusPending, cleaning.StatusCleared}
	propPairs    = [][2]string{{"cat-x", "cat-y"}, {"cat-y", "cat-x"}, {"cat-x", "cat-z"}}
)

// drawFindings draws up to 12 findings with distinct IDs.
func drawFindings(t *rapid.T) []cleaning.StoredFinding {
	n := rapid.IntRange(0, 12).Draw(t, "n")
	out := make([]cleaning.StoredFinding, n)
	for i := range out {
		pair := rapid.SampledFrom(propPairs).Draw(t, "pair")
		out[i] = cleaningtest.ContractFinding(i, rapid.SampledFrom(propSignals).Draw(t, "signal"),
			rapid.SampledFrom(propStatuses).Draw(t, "status"), pair[0], pair[1])
	}
	return out
}

func propCtx(t *rapid.T) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	return ctx
}

func findingIDs(fs []cleaning.StoredFinding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("ListFindings pages through every match exactly once, in ID order, for any filter and page size", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx := propCtx(t)
			stored := drawFindings(t)
			fake := &cleaningtest.Fake{}
			if err := fake.ApplyRun(ctx, "tenant-a", cleaning.RunWrite{Run: cleaningtest.ContractRun(1), Upserts: cleaningtest.NewUpserts(stored...)}); err != nil {
				t.Fatalf("ApplyRun: %v", err)
			}
			filter := cleaning.FindingFilter{PageSize: rapid.IntRange(1, 5).Draw(t, "pageSize")}
			if rapid.Bool().Draw(t, "bySignal") {
				filter.Signal = rapid.SampledFrom(propSignals).Draw(t, "signal")
			}
			if rapid.Bool().Draw(t, "byStatus") {
				filter.Status = rapid.SampledFrom(propStatuses).Draw(t, "status")
			}
			if rapid.Bool().Draw(t, "byPair") {
				pair := rapid.SampledFrom(propPairs).Draw(t, "pair")
				filter.SourceCatalog, filter.TargetCatalog = pair[0], pair[1]
			}

			var want []string
			for _, f := range stored {
				samePair := filter.SourceCatalog == "" ||
					(f.SourceCatalog == filter.SourceCatalog && f.TargetCatalog == filter.TargetCatalog) ||
					(f.SourceCatalog == filter.TargetCatalog && f.TargetCatalog == filter.SourceCatalog)
				if (filter.Signal == "" || f.Signal == filter.Signal) && (filter.Status == "" || f.Status == filter.Status) && samePair {
					want = append(want, f.ID)
				}
			}
			slices.Sort(want)

			var got []string
			for range len(stored) + 1 {
				page, next, err := fake.ListFindings(ctx, "tenant-a", filter)
				if err != nil {
					t.Fatalf("ListFindings: %v", err)
				}
				if len(page) > filter.PageSize {
					t.Fatalf("page of %d exceeds page size %d", len(page), filter.PageSize)
				}
				got = append(got, findingIDs(page)...)
				if next == "" {
					break
				}
				filter.PageToken = next
			}
			if !slices.Equal(got, want) {
				t.Fatalf("paged IDs %v, want %v", got, want)
			}
		})
	})

	It("ApplyRun stores every finding of a valid run and nothing of a refused one", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx := propCtx(t)
			fake := &cleaningtest.Fake{}
			before := drawFindings(t)
			if err := fake.ApplyRun(ctx, "tenant-a", cleaning.RunWrite{Run: cleaningtest.ContractRun(1), Upserts: cleaningtest.NewUpserts(before...)}); err != nil {
				t.Fatalf("ApplyRun: %v", err)
			}
			run := drawFindings(t)
			for i := range run {
				run[i].Status = rapid.SampledFrom(propStatuses).Draw(t, "newStatus")
			}
			refused := len(run) > 0 && rapid.Bool().Draw(t, "refused")
			if refused {
				run[rapid.IntRange(0, len(run)-1).Draw(t, "bad")].Status = "bogus"
			}

			// Each upsert expects the state its finding was stored with, or
			// absence for a new one, so only the injected status can refuse it.
			stateOf := map[string]string{}
			for _, f := range before {
				stateOf[f.ID] = cleaning.StateToken(f)
			}
			upserts := make([]cleaning.RunUpsert, len(run))
			for i, f := range run {
				upserts[i] = cleaning.RunUpsert{Finding: f, ExpectState: stateOf[f.ID]}
			}
			err := fake.ApplyRun(ctx, "tenant-a", cleaning.RunWrite{Run: cleaningtest.ContractRun(2), Upserts: upserts})
			if refused != (err != nil) {
				t.Fatalf("refused=%v but ApplyRun returned %v", refused, err)
			}
			want := map[string]cleaning.Status{}
			for _, f := range before {
				want[f.ID] = f.Status
			}
			if !refused {
				for _, f := range run {
					want[f.ID] = f.Status
				}
			}
			all, err := fake.Findings(ctx, "tenant-a", cleaning.Scope{})
			if err != nil {
				t.Fatalf("Findings: %v", err)
			}
			if len(all) != len(want) {
				t.Fatalf("stored %d findings, want %d", len(all), len(want))
			}
			for _, f := range all {
				if want[f.ID] != f.Status {
					t.Fatalf("finding %s has status %q, want %q", f.ID, f.Status, want[f.ID])
				}
			}
			wantRuns := 2
			if refused {
				wantRuns = 1
			}
			if n := len(fake.Runs("tenant-a")); n != wantRuns {
				t.Fatalf("stored %d runs, want %d", n, wantRuns)
			}
		})
	})
})
