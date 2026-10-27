package retentiontest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// HoldStoreEnv is one spec's private store plus its privileged seeding path.
type HoldStoreEnv struct {
	// Store is the store under test, as the restricted application role would
	// use it (tenant-scoped, subject to row-level security).
	Store retention.HoldStore
	// Seed stores h as given, bypassing Store's checks: any tenant or none
	// (a global hold), any CreatedAt, ReleasedAt and ExpiresAt. h.ID is
	// ignored; the contract never relies on a seeded ID. Every Seed call
	// precedes the first Store call of the spec.
	Seed func(h retention.Hold)
}

// HoldStoreSetup returns a store for one spec. It runs inside a BeforeEach, so
// it should register teardown with DeferCleanup. The store may share its
// backing data with other specs and suites: the contract ends every hold name
// and tenant ID with the per-spec random token it passes in, asserts only on
// holds carrying it, and never lists the whole store. Teardown can therefore
// delete exactly this spec's holds by token. Global holds carry the token as
// their JobID too, so a leaked one matches no real candidate.
type HoldStoreSetup func(token string) HoldStoreEnv

// contractNow is the instant the contract passes to Active. Every fixture
// time is a whole number of hours from it.
var contractNow = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

// HoldStoreContract registers the specs every retention.HoldStore must pass.
// Run it against Fake in this package's suite and against the Postgres store
// in pkg/retention's integration suite, so the two cannot drift: a behavior
// the contract asserts is one Postgres exhibits. It covers:
//
//   - tenant scope: every method refuses a context without a tenant
//     (db.ErrTenantRequired) and leaves the store unchanged;
//   - row-level security: List and Active show the caller's holds and global
//     ones, never another tenant's; Create refuses a hold for another tenant
//     or for no tenant; Release by another tenant is a silent no-op;
//   - Create: returns the stored hold with a store-assigned ID and CreatedAt
//     and no release data, ignoring caller-supplied values for those; refuses
//     a name that has an unreleased hold, even in another tenant; accepts it
//     again once released;
//   - Release: marks the hold released by the caller; unknown names and
//     already released holds are silent no-ops that keep the first release;
//     a visible global hold cannot be released and stays active;
//   - List and Active: List keeps released and expired holds, Active drops
//     them (a hold expiring exactly at now is expired); both order newest
//     CreatedAt first.
//
// It does not cover what is specific to Fake: the ID format, ordering of equal
// CreatedAt values, the Now clock, Seed-once semantics and the Funcs.
func HoldStoreContract(setup HoldStoreSetup) {
	ginkgo.Describe("HoldStore contract", func() {
		var (
			env            HoldStoreEnv
			tokenStr       string
			ctxA, ctxB     context.Context
			tenA, tenB     string
			scopeA, scopeB retention.HoldScope
			globalScope    retention.HoldScope // no tenant; JobID is the token
			nameOf         func(string) string
			ownHolds       func([]retention.Hold) []retention.Hold
			ownNames       func([]retention.Hold) []string
			listA, activeA func() []retention.Hold
			releasedTime   time.Time
			hoursFromNow   func(int) *time.Time
			seedA          func(name string, mutate func(*retention.Hold))
			createA        func(name string) retention.Hold
		)

		ginkgo.BeforeEach(func() {
			raw := make([]byte, 6)
			_, err := rand.Read(raw)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			tokenStr = hex.EncodeToString(raw)
			tenA, tenB = "contract-a-"+tokenStr, "contract-b-"+tokenStr // end with the token
			scopeA, scopeB = retention.HoldScope{TenantID: tenA}, retention.HoldScope{TenantID: tenB}
			// A global hold matches every tenant's data, so a copy leaked into a
			// shared database must match none: its JobID is this spec's token.
			globalScope = retention.HoldScope{JobID: tokenStr}
			nameOf = func(n string) string { return "contract-" + n + "-" + tokenStr }
			mustCtx := func(id string) context.Context {
				ctx, err := tenant.WithTenant(context.Background(), id)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return ctx
			}
			ctxA, ctxB = mustCtx(tenA), mustCtx(tenB)
			releasedTime = contractNow.Add(-time.Hour)
			hoursFromNow = func(h int) *time.Time {
				t := contractNow.Add(time.Duration(h) * time.Hour)
				return &t
			}
			ownHolds = func(in []retention.Hold) []retention.Hold {
				var out []retention.Hold
				for _, h := range in {
					if strings.HasSuffix(h.Name, tokenStr) {
						out = append(out, h)
					}
				}
				return out
			}
			ownNames = func(in []retention.Hold) []string {
				var out []string
				for _, h := range ownHolds(in) {
					out = append(out, strings.TrimSuffix(strings.TrimPrefix(h.Name, "contract-"), "-"+tokenStr))
				}
				return out
			}
			env = setup(tokenStr)
			listA = func() []retention.Hold {
				got, err := env.Store.List(ctxA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return ownHolds(got)
			}
			activeA = func() []retention.Hold {
				got, err := env.Store.Active(ctxA, contractNow)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return ownHolds(got)
			}
			seedA = func(name string, mutate func(*retention.Hold)) {
				h := retention.Hold{Name: nameOf(name), Scope: scopeA, CreatedBy: "seed", CreatedAt: contractNow.Add(-24 * time.Hour)}
				if mutate != nil {
					mutate(&h)
				}
				env.Seed(h)
			}
			createA = func(name string) retention.Hold {
				created, err := env.Store.Create(ctxA, retention.Hold{Name: nameOf(name), Scope: scopeA, CreatedBy: "alice"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return created
			}
		})

		ginkgo.DescribeTable("refuses a context without a tenant and changes nothing",
			func(call func(store retention.HoldStore, seeded, fresh string) error) {
				seedA("seeded", nil)
				err := call(env.Store, nameOf("seeded"), nameOf("new"))
				gomega.Expect(err).To(gomega.MatchError(db.ErrTenantRequired))
				gomega.Expect(ownNames(listA())).To(gomega.Equal([]string{"seeded"}))
				gomega.Expect(listA()[0].ReleasedAt).To(gomega.BeNil())
			},
			ginkgo.Entry("Create", func(s retention.HoldStore, _, fresh string) error {
				_, err := s.Create(context.Background(), retention.Hold{Name: fresh})
				return err
			}),
			ginkgo.Entry("Release", func(s retention.HoldStore, seeded, _ string) error {
				return s.Release(context.Background(), seeded, "me")
			}),
			ginkgo.Entry("List", func(s retention.HoldStore, _, _ string) error {
				_, err := s.List(context.Background())
				return err
			}),
			ginkgo.Entry("Active", func(s retention.HoldStore, _, _ string) error {
				_, err := s.Active(context.Background(), contractNow)
				return err
			}),
		)

		ginkgo.Describe("Create", func() {
			ginkgo.It("returns the hold as stored, ignoring caller-supplied ID, CreatedAt and release data", func() {
				cutoff := *hoursFromNow(-48)
				expires := *hoursFromNow(48)
				wrongTime := time.Unix(5, 0).UTC()
				got, err := env.Store.Create(ctxA, retention.Hold{
					ID: "caller-id", Name: nameOf("full"), CreatedBy: "alice", CreatedAt: wrongTime,
					ReleasedAt: &wrongTime, ReleasedBy: "caller",
					Scope:     retention.HoldScope{TenantID: tenA, JobID: "job-1", CreatedBefore: &cutoff},
					ExpiresAt: &expires,
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(got.ID).NotTo(gomega.BeEmpty())
				gomega.Expect(got.ID).NotTo(gomega.Equal("caller-id"))
				gomega.Expect(got.Name).To(gomega.Equal(nameOf("full")))
				gomega.Expect(got.CreatedBy).To(gomega.Equal("alice"))
				gomega.Expect(got.CreatedAt).NotTo(gomega.BeTemporally("==", wrongTime))
				gomega.Expect(got.CreatedAt).NotTo(gomega.BeZero())
				gomega.Expect(got.Scope.TenantID).To(gomega.Equal(tenA))
				gomega.Expect(got.Scope.JobID).To(gomega.Equal("job-1"))
				gomega.Expect(got.Scope.CreatedBefore).NotTo(gomega.BeNil())
				gomega.Expect(*got.Scope.CreatedBefore).To(gomega.BeTemporally("==", cutoff))
				gomega.Expect(got.ExpiresAt).NotTo(gomega.BeNil())
				gomega.Expect(*got.ExpiresAt).To(gomega.BeTemporally("==", expires))
				gomega.Expect(got.ReleasedAt).To(gomega.BeNil())
				gomega.Expect(got.ReleasedBy).To(gomega.BeEmpty())

				listed := listA()
				gomega.Expect(listed).To(gomega.HaveLen(1))
				gomega.Expect(listed[0].ID).To(gomega.Equal(got.ID))
			})

			ginkgo.It("gives distinct holds distinct IDs", func() {
				a, b := createA("one"), createA("two")
				gomega.Expect(a.ID).NotTo(gomega.Equal(b.ID))
			})

			ginkgo.DescribeTable("refuses a hold whose scope tenant is not the caller's and stores nothing",
				func(scope func() retention.HoldScope) {
					_, err := env.Store.Create(ctxB, retention.Hold{Name: nameOf("refused"), CreatedBy: "b", Scope: scope()})
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).NotTo(gomega.MatchError(db.ErrTenantRequired))
					for _, ctx := range []context.Context{ctxA, ctxB} {
						got, err := env.Store.List(ctx)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						gomega.Expect(ownHolds(got)).To(gomega.BeEmpty())
					}
				},
				ginkgo.Entry("another tenant", func() retention.HoldScope { return scopeA }),
				ginkgo.Entry("no tenant (a global hold)", func() retention.HoldScope { return retention.HoldScope{} }),
			)

			ginkgo.It("refuses a name that has an unreleased hold, in the same or another tenant, and stores nothing new", func() {
				first := createA("dup")
				_, err := env.Store.Create(ctxA, retention.Hold{Name: nameOf("dup"), Scope: scopeA, CreatedBy: "alice"})
				gomega.Expect(err).To(gomega.HaveOccurred())
				_, err = env.Store.Create(ctxB, retention.Hold{Name: nameOf("dup"), Scope: scopeB, CreatedBy: "bob"})
				gomega.Expect(err).To(gomega.HaveOccurred())
				listed := listA()
				gomega.Expect(listed).To(gomega.HaveLen(1))
				gomega.Expect(listed[0].ID).To(gomega.Equal(first.ID))
				got, err := env.Store.List(ctxB)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ownHolds(got)).To(gomega.BeEmpty())
			})

			ginkgo.It("accepts a name again once its earlier hold is released", func() {
				createA("again")
				gomega.Expect(env.Store.Release(ctxA, nameOf("again"), "me")).To(gomega.Succeed())
				_, err := env.Store.Create(ctxA, retention.Hold{Name: nameOf("again"), Scope: scopeA, CreatedBy: "alice"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ownNames(listA())).To(gomega.Equal([]string{"again", "again"}))
				gomega.Expect(ownNames(activeA())).To(gomega.Equal([]string{"again"}))
			})
		})

		ginkgo.Describe("Release", func() {
			ginkgo.It("marks the hold released by the caller, keeps it in List and drops it from Active", func() {
				created := createA("legal")
				gomega.Expect(env.Store.Release(ctxA, nameOf("legal"), "bob")).To(gomega.Succeed())
				listed := listA()
				gomega.Expect(listed).To(gomega.HaveLen(1))
				gomega.Expect(listed[0].ID).To(gomega.Equal(created.ID))
				gomega.Expect(listed[0].ReleasedAt).NotTo(gomega.BeNil())
				gomega.Expect(listed[0].ReleasedBy).To(gomega.Equal("bob"))
				gomega.Expect(activeA()).To(gomega.BeEmpty())
			})

			ginkgo.It("is a silent no-op for an unknown name", func() {
				createA("legal")
				gomega.Expect(env.Store.Release(ctxA, nameOf("nope"), "bob")).To(gomega.Succeed())
				gomega.Expect(ownNames(activeA())).To(gomega.Equal([]string{"legal"}))
			})

			ginkgo.It("is idempotent and keeps the first release's data", func() {
				createA("legal")
				gomega.Expect(env.Store.Release(ctxA, nameOf("legal"), "bob")).To(gomega.Succeed())
				first := listA()
				gomega.Expect(env.Store.Release(ctxA, nameOf("legal"), "carol")).To(gomega.Succeed())
				second := listA()
				gomega.Expect(second).To(gomega.HaveLen(1))
				gomega.Expect(second[0].ReleasedBy).To(gomega.Equal("bob"))
				gomega.Expect(second[0].ReleasedAt).NotTo(gomega.BeNil())
				gomega.Expect(*second[0].ReleasedAt).To(gomega.BeTemporally("==", *first[0].ReleasedAt))
			})

			ginkgo.It("is a silent no-op for another tenant's hold, which stays active", func() {
				createA("legal")
				gomega.Expect(env.Store.Release(ctxB, nameOf("legal"), "mallory")).To(gomega.Succeed())
				active := activeA()
				gomega.Expect(ownNames(active)).To(gomega.Equal([]string{"legal"}))
				gomega.Expect(active[0].ReleasedBy).To(gomega.BeEmpty())
			})

			ginkgo.It("cannot release a visible global hold, which stays active", func() {
				seedA("platform", func(h *retention.Hold) { h.Scope = globalScope })
				gomega.Expect(env.Store.Release(ctxA, nameOf("platform"), "bob")).To(gomega.HaveOccurred())
				active := activeA()
				gomega.Expect(ownNames(active)).To(gomega.Equal([]string{"platform"}))
				gomega.Expect(active[0].ReleasedAt).To(gomega.BeNil())
			})
		})

		ginkgo.Describe("List and Active", func() {
			ginkgo.It("shows the caller's and global holds and hides another tenant's", func() {
				seedA("global", func(h *retention.Hold) { h.Scope = globalScope })
				seedA("own", nil)
				seedA("foreign", func(h *retention.Hold) { h.Scope = scopeB })
				gomega.Expect(ownNames(listA())).To(gomega.ConsistOf("global", "own"))
				gomega.Expect(ownNames(activeA())).To(gomega.ConsistOf("global", "own"))
				gotB, err := env.Store.Active(ctxB, contractNow)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ownNames(gotB)).To(gomega.ConsistOf("global", "foreign"))
			})

			ginkgo.It("keeps released and expired holds in List and drops them from Active", func() {
				seedA("open", nil)
				seedA("unexpired", func(h *retention.Hold) { h.ExpiresAt = hoursFromNow(1) })
				seedA("expired", func(h *retention.Hold) { h.ExpiresAt = hoursFromNow(-1) })
				seedA("released", func(h *retention.Hold) { h.ReleasedAt = &releasedTime; h.ReleasedBy = "x" })
				gomega.Expect(ownNames(listA())).To(gomega.ConsistOf("open", "unexpired", "expired", "released"))
				gomega.Expect(ownNames(activeA())).To(gomega.ConsistOf("open", "unexpired"))
			})

			ginkgo.It("treats a hold expiring exactly at now as expired", func() {
				seedA("edge", func(h *retention.Hold) { h.ExpiresAt = hoursFromNow(0) })
				gomega.Expect(ownNames(listA())).To(gomega.Equal([]string{"edge"}))
				gomega.Expect(activeA()).To(gomega.BeEmpty())
			})

			ginkgo.It("orders newest CreatedAt first", func() {
				seedA("middle", func(h *retention.Hold) { h.CreatedAt = contractNow.Add(-2 * time.Hour) })
				seedA("oldest", func(h *retention.Hold) { h.CreatedAt = contractNow.Add(-3 * time.Hour) })
				seedA("newest", func(h *retention.Hold) { h.CreatedAt = contractNow.Add(-1 * time.Hour) })
				gomega.Expect(ownNames(listA())).To(gomega.Equal([]string{"newest", "middle", "oldest"}))
				gomega.Expect(ownNames(activeA())).To(gomega.Equal([]string{"newest", "middle", "oldest"}))
			})

			ginkgo.It("returns the seeded scope and times intact", func() {
				cutoff := *hoursFromNow(-72)
				seedA("scoped", func(h *retention.Hold) {
					h.Scope = retention.HoldScope{TenantID: tenA, JobID: "job-9", CreatedBefore: &cutoff}
				})
				got := listA()
				gomega.Expect(got).To(gomega.HaveLen(1))
				gomega.Expect(got[0].Scope.JobID).To(gomega.Equal("job-9"))
				gomega.Expect(*got[0].Scope.CreatedBefore).To(gomega.BeTemporally("==", cutoff))
				gomega.Expect(got[0].CreatedBy).To(gomega.Equal("seed"))
			})
		})
	})
}
