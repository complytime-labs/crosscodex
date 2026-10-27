package retentiontest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/retention/retentiontest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestRetentionTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RetentionTest Suite")
}

var errScripted = errors.New("scripted")

func tenantCtx(id string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), id)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

func names(holds []retention.Hold) []string {
	var out []string
	for _, h := range holds {
		out = append(out, h.Name)
	}
	return out
}

// The same specs run against the Postgres store in pkg/retention's
// integration suite.
var _ = Describe("Fake", func() {
	retentiontest.HoldStoreContract(func(string) retentiontest.HoldStoreEnv {
		fake := &retentiontest.Fake{}
		return retentiontest.HoldStoreEnv{
			Store: fake,
			Seed:  func(h retention.Hold) { fake.Seed = append(fake.Seed, h) },
		}
	})
})

// Specs for what only Fake has: its clock, generated IDs, Seed, Funcs and
// copy semantics. Behavior shared with the Postgres store is in the contract.
var _ = Describe("Fake-only behavior", func() {
	var (
		ctxA  context.Context
		fake  *retentiontest.Fake
		clock time.Time
	)

	BeforeEach(func() {
		ctxA = tenantCtx("tenant-a")
		clock = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		fake = &retentiontest.Fake{Now: func() time.Time {
			clock = clock.Add(time.Hour)
			return clock
		}}
	})

	Describe("Now", func() {
		It("stamps CreatedAt on Create and ReleasedAt on Release", func() {
			created, err := fake.Create(ctxA, retention.Hold{Name: "h", Scope: retention.HoldScope{TenantID: "tenant-a"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(created.CreatedAt).To(Equal(clock))
			Expect(fake.Release(ctxA, "h", "me")).To(Succeed())
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(*listed[0].ReleasedAt).To(Equal(clock))
		})

		It("defaults to the wall clock", func() {
			wallClock := &retentiontest.Fake{}
			before := time.Now()
			created, err := wallClock.Create(ctxA, retention.Hold{Name: "h", Scope: retention.HoldScope{TenantID: "tenant-a"}})
			Expect(err).NotTo(HaveOccurred())
			Expect(created.CreatedAt).To(BeTemporally(">=", before))
			Expect(created.CreatedAt).To(BeTemporally("<=", time.Now()))
		})
	})

	Describe("generated IDs", func() {
		scope := retention.HoldScope{TenantID: "tenant-a"}

		It("are sequential", func() {
			a, err := fake.Create(ctxA, retention.Hold{Name: "a", Scope: scope})
			Expect(err).NotTo(HaveOccurred())
			b, err := fake.Create(ctxA, retention.Hold{Name: "b", Scope: scope})
			Expect(err).NotTo(HaveOccurred())
			Expect([]string{a.ID, b.ID}).To(Equal([]string{"hold-1", "hold-2"}))
		})

		It("skip an ID a seeded hold already has", func() {
			fake.Seed = []retention.Hold{{ID: "hold-1", Name: "seeded"}, {ID: "hold-3", Name: "seeded-3"}}
			a, err := fake.Create(ctxA, retention.Hold{Name: "a", Scope: scope})
			Expect(err).NotTo(HaveOccurred())
			b, err := fake.Create(ctxA, retention.Hold{Name: "b", Scope: scope})
			Expect(err).NotTo(HaveOccurred())
			c, err := fake.Create(ctxA, retention.Hold{Name: "c", Scope: scope})
			Expect(err).NotTo(HaveOccurred())
			Expect([]string{a.ID, b.ID, c.ID}).To(Equal([]string{"hold-2", "hold-4", "hold-5"}))
		})
	})

	Describe("ordering", func() {
		It("breaks CreatedAt ties by most recently stored first", func() {
			t1 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			fake.Seed = []retention.Hold{
				{ID: "1", Name: "tie-first", CreatedAt: t1},
				{ID: "2", Name: "tie-last", CreatedAt: t1},
				{ID: "3", Name: "new", CreatedAt: t1.Add(time.Hour)},
			}
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(names(listed)).To(Equal([]string{"new", "tie-last", "tie-first"}))
		})
	})

	Describe("results", func() {
		It("are nil when no hold is visible, as the store returns them", func() {
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(listed).To(BeNil())
			active, err := fake.Active(ctxA, clock)
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeNil())
		})

		It("are copies, so editing one leaves the store intact", func() {
			expires := clock.Add(time.Hour)
			fake.Seed = []retention.Hold{{ID: "1", Name: "h", ExpiresAt: &expires}}
			got, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			got[0].Name = "mutated"
			*got[0].ExpiresAt = clock.Add(-time.Hour)
			again, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(again[0].Name).To(Equal("h"))
			Expect(*again[0].ExpiresAt).To(Equal(clock.Add(time.Hour)))
		})

		It("do not change when the caller edits the hold it passed to Create", func() {
			expires := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
			h := retention.Hold{Name: "m", Scope: retention.HoldScope{TenantID: "tenant-a"}, ExpiresAt: &expires}
			_, err := fake.Create(ctxA, h)
			Expect(err).NotTo(HaveOccurred())
			expires = expires.Add(24 * time.Hour)
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(*listed[0].ExpiresAt).To(Equal(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)))
		})
	})

	Describe("Seed", func() {
		It("is read once, on first use", func() {
			fake.Seed = []retention.Hold{{ID: "1", Name: "first"}}
			_, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			fake.Seed = append(fake.Seed, retention.Hold{ID: "2", Name: "late"})
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(names(listed)).To(Equal([]string{"first"}))
		})

		It("is copied, so editing it after the first call leaves the store intact", func() {
			fake.Seed = []retention.Hold{{ID: "1", Name: "first"}}
			_, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			fake.Seed[0].Name = "mutated"
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(names(listed)).To(Equal([]string{"first"}))
		})
	})

	Describe("scripted calls", func() {
		It("never reach a Func when the context has no tenant", func() {
			reached := false
			fake.CreateFunc = func(context.Context, retention.Hold) (retention.Hold, error) {
				reached = true
				return retention.Hold{}, nil
			}
			fake.ReleaseFunc = func(context.Context, string, string) error { reached = true; return nil }
			fake.ListFunc = func(context.Context) ([]retention.Hold, error) { reached = true; return nil, nil }
			fake.ActiveFunc = func(context.Context, time.Time) ([]retention.Hold, error) { reached = true; return nil, nil }

			_, err := fake.Create(context.Background(), retention.Hold{})
			Expect(err).To(MatchError(db.ErrTenantRequired))
			Expect(fake.Release(context.Background(), "h", "me")).To(MatchError(db.ErrTenantRequired))
			_, err = fake.List(context.Background())
			Expect(err).To(MatchError(db.ErrTenantRequired))
			_, err = fake.Active(context.Background(), clock)
			Expect(err).To(MatchError(db.ErrTenantRequired))
			Expect(reached).To(BeFalse())
		})

		It("let each Func alone decide its method's result and leave the store untouched", func() {
			fake.CreateFunc = func(context.Context, retention.Hold) (retention.Hold, error) {
				return retention.Hold{}, errScripted
			}
			fake.ReleaseFunc = func(context.Context, string, string) error { return errScripted }
			fake.ListFunc = func(context.Context) ([]retention.Hold, error) { return nil, errScripted }
			fake.ActiveFunc = func(context.Context, time.Time) ([]retention.Hold, error) { return nil, errScripted }

			_, err := fake.Create(ctxA, retention.Hold{Name: "h", Scope: retention.HoldScope{TenantID: "tenant-a"}})
			Expect(err).To(MatchError(errScripted))
			Expect(fake.Release(ctxA, "h", "me")).To(MatchError(errScripted))
			_, err = fake.List(ctxA)
			Expect(err).To(MatchError(errScripted))
			_, err = fake.Active(ctxA, clock)
			Expect(err).To(MatchError(errScripted))

			fake.CreateFunc, fake.ReleaseFunc, fake.ListFunc, fake.ActiveFunc = nil, nil, nil, nil
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(listed).To(BeEmpty())
		})

		It("hand a Func the caller's arguments", func() {
			want := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
			fake.ActiveFunc = func(_ context.Context, now time.Time) ([]retention.Hold, error) {
				Expect(now).To(Equal(want))
				return []retention.Hold{{Name: "scripted"}}, nil
			}
			fake.ReleaseFunc = func(_ context.Context, name, by string) error {
				Expect([]string{name, by}).To(Equal([]string{"h", "me"}))
				return nil
			}
			got, err := fake.Active(ctxA, want)
			Expect(err).NotTo(HaveOccurred())
			Expect(names(got)).To(Equal([]string{"scripted"}))
			Expect(fake.Release(ctxA, "h", "me")).To(Succeed())
		})
	})

	Describe("concurrency", func() {
		It("stores every hold created from 20 goroutines under distinct IDs", func() {
			var wg sync.WaitGroup
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					_, err := fake.Create(ctxA, retention.Hold{
						Name:  string(rune('a' + i)),
						Scope: retention.HoldScope{TenantID: "tenant-a"},
					})
					Expect(err).NotTo(HaveOccurred())
					_, err = fake.List(ctxA)
					Expect(err).NotTo(HaveOccurred())
				}()
			}
			wg.Wait()
			listed, err := fake.List(ctxA)
			Expect(err).NotTo(HaveOccurred())
			Expect(listed).To(HaveLen(20))
			ids := map[string]bool{}
			for _, h := range listed {
				ids[h.ID] = true
			}
			Expect(ids).To(HaveLen(20))
		})
	})
})
