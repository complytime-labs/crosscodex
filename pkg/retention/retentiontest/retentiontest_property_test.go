package retentiontest_test

import (
	"context"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/retention/retentiontest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var propNow = time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

// holdKind is the lifecycle state a generated hold is built in. The oracle
// below is a table over kinds, so it does not restate the Active rule
// (Hold.ActiveAt) the Fake is expected to apply.
type holdKind int

const (
	kindOpen      holdKind = iota // no expiry, not released: active
	kindUnexpired                 // expires after now: active
	kindExpired                   // expired before now: not active
	kindReleased                  // released: not active
)

var activeKind = map[holdKind]bool{kindOpen: true, kindUnexpired: true, kindExpired: false, kindReleased: false}

// buildHold returns a hold of the given kind, tenant scope and index.
func buildHold(i int, kind holdKind, scopeTenant string) retention.Hold {
	h := retention.Hold{
		ID:    "seed-" + string(rune('a'+i)),
		Name:  "hold-" + string(rune('a'+i)),
		Scope: retention.HoldScope{TenantID: scopeTenant},
	}
	switch kind {
	case kindUnexpired:
		e := propNow.Add(time.Hour)
		h.ExpiresAt = &e
	case kindExpired:
		e := propNow.Add(-time.Hour)
		h.ExpiresAt = &e
	case kindReleased:
		r := propNow.Add(-time.Hour)
		h.ReleasedAt, h.ReleasedBy = &r, "someone"
	}
	return h
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("Active agrees with a reference filter for any seeded set of holds", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			callerTenant := rapid.SampledFrom([]string{"tenant-a", "tenant-b", "tenant-c"}).Draw(t, "caller")
			ctx, err := tenant.WithTenant(context.Background(), callerTenant)
			if err != nil {
				t.Fatalf("WithTenant(%q): %v", callerTenant, err)
			}

			n := rapid.IntRange(0, 10).Draw(t, "n")
			var seed []retention.Hold
			var wantActive, wantList []string
			for i := 0; i < n; i++ {
				kind := holdKind(rapid.IntRange(int(kindOpen), int(kindReleased)).Draw(t, "kind"))
				scopeTenant := rapid.SampledFrom([]string{"", "tenant-a", "tenant-b"}).Draw(t, "scope")
				h := buildHold(i, kind, scopeTenant)
				seed = append(seed, h)
				if scopeTenant != "" && scopeTenant != callerTenant {
					continue // another tenant's hold: invisible
				}
				wantList = append(wantList, h.Name)
				if activeKind[kind] {
					wantActive = append(wantActive, h.Name)
				}
			}

			fake := &retentiontest.Fake{Seed: seed}
			active, err := fake.Active(ctx, propNow)
			if err != nil {
				t.Fatalf("Active: %v", err)
			}
			list, err := fake.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := sortedNames(active); !slices.Equal(got, sortedStrings(wantActive)) {
				t.Fatalf("Active() = %v, want %v", got, sortedStrings(wantActive))
			}
			if got := sortedNames(list); !slices.Equal(got, sortedStrings(wantList)) {
				t.Fatalf("List() = %v, want %v", got, sortedStrings(wantList))
			}
		})
	})

	It("a hold created under one tenant is never visible to another", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			tenants := []string{"tenant-a", "tenant-b", "tenant-c"}
			owner := rapid.SampledFrom(tenants).Draw(t, "owner")
			other := rapid.SampledFrom(tenants).Filter(func(s string) bool { return s != owner }).Draw(t, "other")
			ownerCtx, err := tenant.WithTenant(context.Background(), owner)
			if err != nil {
				t.Fatalf("WithTenant(%q): %v", owner, err)
			}
			otherCtx, err := tenant.WithTenant(context.Background(), other)
			if err != nil {
				t.Fatalf("WithTenant(%q): %v", other, err)
			}

			fake := &retentiontest.Fake{}
			if _, err := fake.Create(ownerCtx, retention.Hold{Name: "h", Scope: retention.HoldScope{TenantID: owner}}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := fake.Release(otherCtx, "h", "intruder"); err != nil {
				t.Fatalf("Release by other tenant: %v", err)
			}
			seen, err := fake.List(otherCtx)
			if err != nil || len(seen) != 0 {
				t.Fatalf("List(other) = %v, %v; want no holds", seen, err)
			}
			active, err := fake.Active(ownerCtx, time.Now())
			if err != nil || len(active) != 1 {
				t.Fatalf("Active(owner) = %v, %v; want the hold still active", active, err)
			}
		})
	})
})

func sortedNames(holds []retention.Hold) []string {
	var out []string
	for _, h := range holds {
		out = append(out, h.Name)
	}
	return sortedStrings(out)
}

func sortedStrings(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
