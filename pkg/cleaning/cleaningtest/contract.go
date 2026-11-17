package cleaningtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// FindingStoreEnv is one spec's store plus its privileged seeding path.
type FindingStoreEnv struct {
	// Store is the store under test, as the restricted application role would
	// use it (tenant-scoped, subject to row-level security).
	Store cleaning.FindingStore
	// SeedReviewer registers reviewerID in tenantID with status "active" or
	// "deleted", as 'crosscodexd admin reviewer add' (and 'delete') would.
	// Every SeedReviewer call precedes the first Store call of the spec.
	SeedReviewer func(tenantID, reviewerID, status string)
}

// FindingStoreSetup returns a store for one spec. It runs inside a
// BeforeEach, so it should register teardown with DeferCleanup. Both tenants
// ContractTenants(token) names must exist in the store before it returns.
// The store may share its backing data with other specs: the contract writes
// only to those two tenants.
type FindingStoreSetup func(token string) FindingStoreEnv

// ContractTenants returns the two tenant IDs the contract uses for the spec
// whose random token is token.
func ContractTenants(token string) (a, b string) {
	return "cleaning-a-" + token, "cleaning-b-" + token
}

// contractTime is the base of every fixture time; all are whole seconds, so
// they survive the database's microsecond precision unchanged.
var contractTime = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

// ContractFinding returns a valid finding numbered n, with the given signal,
// status and catalogs: one concerned verdict that is also the target, two
// edges, and GraphApplied true. A job-level signal's finding instead names
// that verdict's job in its evidence and concerns no verdict. Its ID is
// FindingID, and its EvidenceHash matches its evidence. Distinct n give
// distinct IDs.
func ContractFinding(n int, signal cleaning.Signal, status cleaning.Status, source, target string) cleaning.StoredFinding {
	key := cleaning.VerdictKey{JobID: fmt.Sprintf("job-%d", n), SourceID: fmt.Sprintf("s%d", n), TargetID: fmt.Sprintf("t%d", n)}
	agreement := 0.5
	edges := []string{fmt.Sprintf("edge-%d-a", n), fmt.Sprintf("edge-%d-b", n)}
	ev := cleaning.Evidence{
		Verdicts: []cleaning.VerdictEvidence{{
			Key: key, JobCompletedAt: contractTime.Add(time.Duration(n) * time.Hour),
			SourceIdentifier: "AC-1", SourceLabel: "ac-1", SourceTitle: "Policy", SourceCatalog: source,
			TargetIdentifier: "PO.1", TargetLabel: "po.1", TargetTitle: "Prepare", TargetCatalog: target,
			Relationship: "EQUIVALENT_TO", Agreement: &agreement, EdgeIDs: edges,
		}},
		Detail: map[string]string{"n": fmt.Sprint(n)},
	}
	concerns := []cleaning.VerdictKey{key}
	if signal.JobLevel() {
		ev = cleaning.Evidence{
			Job: &cleaning.JobEvidence{
				ID: key.JobID, CompletedAt: contractTime.Add(time.Duration(n) * time.Hour),
				SourceCatalog: source, TargetCatalog: target, VerdictsInScope: 1,
			},
			Detail: ev.Detail,
		}
		concerns = nil
	}
	hash, err := cleaning.EvidenceHash(ev)
	if err != nil {
		// Unreachable: the evidence holds no value JSON cannot encode. A panic
		// rather than Gomega keeps the helper usable inside rapid.Check.
		panic(fmt.Sprintf("cleaningtest: hash contract evidence: %v", err))
	}
	return cleaning.StoredFinding{
		ID:     cleaning.FindingID(cleaning.Finding{Signal: signal, Concerns: concerns, Evidence: ev}),
		Signal: signal, Evidence: ev, EvidenceHash: hash,
		Concerns: concerns, Targets: []cleaning.VerdictKey{key}, EdgeIDs: edges,
		Status: status, GraphApplied: true, SourceCatalog: source, TargetCatalog: target,
	}
}

// errContaining matches an error whose message contains substr.
func errContaining(substr string) gomegatypes.GomegaMatcher {
	return gomega.MatchError(gomega.ContainSubstring(substr))
}

// NewUpserts returns fs as RunUpserts of findings not stored yet.
func NewUpserts(fs ...cleaning.StoredFinding) []cleaning.RunUpsert {
	out := make([]cleaning.RunUpsert, len(fs))
	for i, f := range fs {
		out[i] = cleaning.RunUpsert{Finding: f}
	}
	return out
}

// FindingStoreContract registers the specs every cleaning.FindingStore must
// pass. Run it against Fake in this package's suite and against the Postgres
// store in pkg/cleaning's integration suite, so the two cannot drift: a
// behavior the contract asserts is one Postgres exhibits. It covers:
//
//   - tenant scope: every method refuses a context without a tenant
//     (tenant.ErrNoTenant) or with a tenant other than tenantID, and leaves
//     the store unchanged; another tenant's finding is ErrFindingNotFound;
//   - ApplyRun: creates or updates each finding against its expected state
//     (a stale state, or a "must not exist" for a stored finding, is
//     ErrStateChanged), round-trips every field (a job-level finding too,
//     with targets and edge IDs each over 2 MiB), stamps UpdatedAt, and
//     writes nothing, with an actionable message, when any finding or
//     decision is refused: what StoredFinding.Validate refuses (unknown
//     signal or status, a subject that does not match the signal, an ID that
//     is not FindingID, an invalid verdict key, evidence over its size
//     bound), a decision on an unknown finding, what
//     Decision.Validate refuses, a decision not made by policy, or a decision
//     ID already stored;
//   - Decide: records the decision byte for byte and moves the finding to
//     next with GraphApplied false; a stale state token, an unregistered or
//     deleted reviewer, an unknown finding, a next for another finding or
//     one Validate refuses, a policy decision and a run ID are refused with
//     the store unchanged;
//   - ListFindings: paging returns every match once, in ID order; filters on
//     signal, status and the unordered catalog pair; a one-sided pair or a
//     non-positive page size is refused; Findings applies the scope the same
//     way;
//   - Unprojected and MarkProjected round-trip; PendingCount counts pending.
//
// The targets and edge_ids bound (128 MiB) is not in the contract, so the
// Fake's run never builds such a value: pkg/cleaning checks it at a lowered
// limit on both stores, and its integration suite once at the real limit on
// Postgres.
//
// CompletedJobs is not covered: it reads jobs the Fake holds pre-parsed, so
// its parsing is specified by the Postgres store's own specs.
func FindingStoreContract(setup FindingStoreSetup) {
	ginkgo.Describe("FindingStore contract", func() {
		var (
			env        FindingStoreEnv
			store      cleaning.FindingStore
			tenA, tenB string
			ctxA, ctxB context.Context
			reviewer   string
		)

		ginkgo.BeforeEach(func() {
			raw := make([]byte, 6)
			_, err := rand.Read(raw)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			token := hex.EncodeToString(raw)
			tenA, tenB = ContractTenants(token)
			reviewer = "alice"
			env = setup(token)
			store = env.Store
			env.SeedReviewer(tenA, reviewer, "active")
			env.SeedReviewer(tenB, reviewer, "active")
			env.SeedReviewer(tenA, "gone", "deleted")
			ctxA, err = tenant.WithTenant(context.Background(), tenA)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ctxB, err = tenant.WithTenant(context.Background(), tenB)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		// apply creates fs, none of which may be stored yet.
		apply := func(ctx context.Context, tenantID string, fs ...cleaning.StoredFinding) {
			gomega.Expect(store.ApplyRun(ctx, tenantID, cleaning.RunWrite{Upserts: NewUpserts(fs...)})).To(gomega.Succeed())
		}
		get := func(id string) (cleaning.StoredFinding, []cleaning.Decision) {
			f, ds, err := store.GetFinding(ctxA, tenA, id)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return f, ds
		}
		// withoutStamp returns f as written: UpdatedAt is the store's.
		withoutStamp := func(f cleaning.StoredFinding) cleaning.StoredFinding {
			f.UpdatedAt = time.Time{}
			return f
		}
		allA := func() []cleaning.StoredFinding {
			fs, err := store.Findings(ctxA, tenA, cleaning.Scope{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return fs
		}
		ids := func(fs []cleaning.StoredFinding) []string {
			out := make([]string, len(fs))
			for i, f := range fs {
				out[i] = f.ID
			}
			return out
		}
		decision := func(id string, f cleaning.StoredFinding, verdict string) cleaning.Decision {
			return cleaning.Decision{
				ID: id, FindingID: f.ID, Verdict: verdict, DecidedBy: reviewer, Rationale: "looked at it",
				Request: []byte(`{"finding":"` + f.ID + `"}`), Attestation: []byte{0x00, 0xff, 0x10},
				DecidedAt: contractTime.Add(time.Minute),
			}
		}

		ginkgo.Describe("tenant scope", func() {
			var seeded cleaning.StoredFinding

			ginkgo.BeforeEach(func() {
				seeded = ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusPending, "cat-x", "cat-y")
				apply(ctxA, tenA, seeded)
			})

			calls := map[string]func(ctx context.Context, tenantID string) error{
				"CompletedJobs": func(ctx context.Context, t string) error { _, err := store.CompletedJobs(ctx, t); return err },
				"Findings": func(ctx context.Context, t string) error {
					_, err := store.Findings(ctx, t, cleaning.Scope{})
					return err
				},
				"ListFindings": func(ctx context.Context, t string) error {
					_, _, err := store.ListFindings(ctx, t, cleaning.FindingFilter{PageSize: 10})
					return err
				},
				"GetFinding": func(ctx context.Context, t string) error {
					_, _, err := store.GetFinding(ctx, t, seeded.ID)
					return err
				},
				"ApplyRun": func(ctx context.Context, t string) error {
					changed := seeded
					changed.Status = cleaning.StatusCleared
					return store.ApplyRun(ctx, t, cleaning.RunWrite{Upserts: []cleaning.RunUpsert{
						{Finding: changed, ExpectState: cleaning.StateToken(seeded)},
					}})
				},
				"Decide": func(ctx context.Context, t string) error {
					next := seeded
					next.Status = cleaning.StatusDismissed
					return store.Decide(ctx, t, decision("d-scope", seeded, cleaning.VerdictDismiss), cleaning.StateToken(seeded), next)
				},
				"Unprojected": func(ctx context.Context, t string) error { _, err := store.Unprojected(ctx, t); return err },
				"MarkProjected": func(ctx context.Context, t string) error {
					return store.MarkProjected(ctx, t, seeded.ID, []string{"x"})
				},
				"PendingCount": func(ctx context.Context, t string) error { _, err := store.PendingCount(ctx, t); return err },
			}
			names := make([]string, 0, len(calls))
			for name := range calls {
				names = append(names, name)
			}
			slices.Sort(names)

			for _, name := range names {
				ginkgo.It(name+" refuses a context without a tenant and changes nothing", func() {
					gomega.Expect(calls[name](context.Background(), tenA)).To(gomega.MatchError(tenant.ErrNoTenant))
					f, ds := get(seeded.ID)
					gomega.Expect(withoutStamp(f)).To(gomega.Equal(seeded))
					gomega.Expect(ds).To(gomega.BeEmpty())
				})
				ginkgo.It(name+" refuses a tenant argument other than the context's and changes nothing", func() {
					gomega.Expect(calls[name](ctxB, tenA)).To(errContaining("does not match"))
					f, ds := get(seeded.ID)
					gomega.Expect(withoutStamp(f)).To(gomega.Equal(seeded))
					gomega.Expect(ds).To(gomega.BeEmpty())
				})
			}

			ginkgo.It("hides one tenant's finding from another", func() {
				_, _, err := store.GetFinding(ctxB, tenB, seeded.ID)
				gomega.Expect(err).To(gomega.MatchError(cleaning.ErrFindingNotFound))
				fs, err := store.Findings(ctxB, tenB, cleaning.Scope{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fs).To(gomega.BeEmpty())
				n, err := store.PendingCount(ctxB, tenB)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(n).To(gomega.BeZero())
			})

			ginkgo.It("does not let another tenant decide or project the finding", func() {
				next := seeded
				next.Status = cleaning.StatusDismissed
				gomega.Expect(store.Decide(ctxB, tenB, decision("d-cross", seeded, cleaning.VerdictDismiss), cleaning.StateToken(seeded), next)).
					To(gomega.MatchError(cleaning.ErrFindingNotFound))
				gomega.Expect(store.MarkProjected(ctxB, tenB, seeded.ID, []string{"x"})).To(gomega.MatchError(cleaning.ErrFindingNotFound))
				f, ds := get(seeded.ID)
				gomega.Expect(withoutStamp(f)).To(gomega.Equal(seeded))
				gomega.Expect(ds).To(gomega.BeEmpty())
			})

			ginkgo.It("keeps one tenant's finding apart from the same ID in another", func() {
				other := seeded
				other.Status = cleaning.StatusReported
				apply(ctxB, tenB, other)
				f, _ := get(seeded.ID)
				gomega.Expect(f.Status).To(gomega.Equal(cleaning.StatusPending))
			})
		})

		ginkgo.Describe("ApplyRun", func() {
			ginkgo.It("round-trips every field and stamps UpdatedAt", func() {
				f := ContractFinding(1, cleaning.SignalEquivalenceFanout, cleaning.StatusRetired, "cat-x", "cat-y")
				f.GraphApplied = false
				f.Targets = nil
				before := time.Now().Add(-time.Minute)
				apply(ctxA, tenA, f)
				got, ds := get(f.ID)
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(f))
				gomega.Expect(got.UpdatedAt).To(gomega.BeTemporally(">=", before))
				gomega.Expect(ds).To(gomega.BeEmpty())
			})

			ginkgo.It("round-trips a job-level finding", func() {
				f := ContractFinding(1, cleaning.SignalStalePrompt, cleaning.StatusRetired, "cat-x", "cat-y")
				apply(ctxA, tenA, f)
				got, _ := get(f.ID)
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(f))
			})

			ginkgo.It("round-trips a job-level finding whose targets and edge IDs each exceed 1 MiB", func() {
				f := ContractFinding(1, cleaning.SignalStaleModel, cleaning.StatusRetired, "cat-x", "cat-y")
				f.Targets, f.EdgeIDs = nil, nil
				for i := range 20000 {
					f.Targets = append(f.Targets, cleaning.VerdictKey{
						JobID: f.Evidence.Job.ID, SourceID: fmt.Sprintf("source-control-%032d", i), TargetID: fmt.Sprintf("target-control-%032d", i),
					})
					f.EdgeIDs = append(f.EdgeIDs, fmt.Sprintf("edge-%0100d", i))
				}
				f.Evidence.Job.VerdictsInScope = len(f.Targets)
				hash, err := cleaning.EvidenceHash(f.Evidence)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				f.EvidenceHash = hash
				for name, v := range map[string]any{"targets": f.Targets, "edge_ids": f.EdgeIDs} {
					b, err := json.Marshal(v)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(len(b)).To(gomega.BeNumerically(">", 2<<20), name)
				}
				apply(ctxA, tenA, f)
				got, _ := get(f.ID)
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(f))
			})

			ginkgo.It("updates the one row when the same ID is upserted again", func() {
				f := ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusReported, "cat-x", "cat-y")
				apply(ctxA, tenA, f)
				changed := f
				changed.Status = cleaning.StatusPending
				changed.Evidence.Detail = map[string]string{"n": "changed"}
				hash, err := cleaning.EvidenceHash(changed.Evidence)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				changed.EvidenceHash = hash
				gomega.Expect(store.ApplyRun(ctxA, tenA, cleaning.RunWrite{Upserts: []cleaning.RunUpsert{
					{Finding: changed, ExpectState: cleaning.StateToken(f)},
				}})).To(gomega.Succeed())
				all := allA()
				gomega.Expect(all).To(gomega.HaveLen(1))
				gomega.Expect(withoutStamp(all[0])).To(gomega.Equal(changed))
			})

			ginkgo.It("records policy decisions with their run ID", func() {
				f := ContractFinding(1, cleaning.SignalConflictsWith, cleaning.StatusRetired, "cat-x", "cat-y")
				d := decision("d-policy", f, cleaning.VerdictRetire)
				d.DecidedBy, d.RunID = cleaning.DecidedByPolicy, "plan-token"
				gomega.Expect(store.ApplyRun(ctxA, tenA, cleaning.RunWrite{
					Upserts: NewUpserts(f), Decisions: []cleaning.Decision{d},
				})).To(gomega.Succeed())
				_, ds := get(f.ID)
				gomega.Expect(ds).To(gomega.Equal([]cleaning.Decision{d}))
			})

			ginkgo.DescribeTable("writes nothing when any part of the run is refused",
				func(mutate func(w *cleaning.RunWrite), want gomegatypes.GomegaMatcher) {
					kept := ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusReported, "cat-x", "cat-y")
					apply(ctxA, tenA, kept)
					d0 := decision("d-0", kept, cleaning.VerdictRetire)
					d0.DecidedBy, d0.RunID = cleaning.DecidedByPolicy, "run-0"
					gomega.Expect(store.ApplyRun(ctxA, tenA, cleaning.RunWrite{Decisions: []cleaning.Decision{d0}})).To(gomega.Succeed())

					changed := kept
					changed.Status = cleaning.StatusCleared
					fresh := ContractFinding(2, cleaning.SignalStaleModel, cleaning.StatusReported, "cat-x", "cat-y")
					d1 := decision("d-1", fresh, cleaning.VerdictRetire)
					d1.DecidedBy, d1.RunID = cleaning.DecidedByPolicy, "run-1"
					w := cleaning.RunWrite{
						Upserts: []cleaning.RunUpsert{
							{Finding: changed, ExpectState: cleaning.StateToken(kept)},
							{Finding: fresh},
						},
						Decisions: []cleaning.Decision{d1},
					}
					mutate(&w)

					gomega.Expect(store.ApplyRun(ctxA, tenA, w)).To(want)
					all := allA()
					gomega.Expect(all).To(gomega.HaveLen(1))
					gomega.Expect(withoutStamp(all[0])).To(gomega.Equal(kept))
					_, ds := get(kept.ID)
					gomega.Expect(ds).To(gomega.Equal([]cleaning.Decision{d0}))
				},
				ginkgo.Entry("a finding with an unknown status",
					func(w *cleaning.RunWrite) { w.Upserts[1].Finding.Status = "bogus" },
					errContaining(`unknown status "bogus"`)),
				ginkgo.Entry("a finding with an unknown signal", func(w *cleaning.RunWrite) {
					f := &w.Upserts[1].Finding
					f.Signal = "bogus"
					f.ID = cleaning.FindingID(cleaning.Finding{Signal: "bogus", Concerns: f.Concerns})
					w.Decisions = nil
				}, errContaining(`unknown signal "bogus"`)),
				ginkgo.Entry("a finding whose ID is not FindingID of its signal and concerns", func(w *cleaning.RunWrite) {
					w.Upserts[1].Finding.Signal = cleaning.SignalStalePrompt
					w.Decisions = nil
				}, errContaining("build stored findings with FindingID")),
				ginkgo.Entry("a job-level signal keyed by verdicts", func(w *cleaning.RunWrite) {
					f := &w.Upserts[1].Finding
					f.Evidence.Job, f.Concerns = nil, f.Targets
					f.ID = cleaning.FindingID(cleaning.Finding{Signal: f.Signal, Concerns: f.Concerns})
					hash, err := cleaning.EvidenceHash(f.Evidence)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.EvidenceHash = hash
				}, gomega.And(errContaining("signal stale_model is raised per job"), errContaining("nothing was written"))),
				ginkgo.Entry("a target with an empty key part", func(w *cleaning.RunWrite) {
					w.Upserts[1].Finding.Targets[0].JobID = ""
				}, gomega.And(gomega.MatchError(cleaning.ErrInvalidVerdictKey), errContaining("target 0"),
					errContaining("nothing was written"))),
				ginkgo.Entry("evidence over the size bound", func(w *cleaning.RunWrite) {
					f := &w.Upserts[1].Finding
					f.Evidence.Detail = map[string]string{"x": strings.Repeat("x", 1<<20)}
					hash, err := cleaning.EvidenceHash(f.Evidence)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					f.EvidenceHash = hash
				}, gomega.And(gomega.MatchError(cleaning.ErrFindingTooLarge), errContaining("the evidence column would hold"),
					errContaining("nothing was written"))),
				ginkgo.Entry("a stale expected state", func(w *cleaning.RunWrite) { w.Upserts[0].ExpectState = "stale" },
					gomega.And(gomega.MatchError(cleaning.ErrStateChanged), errContaining("Preview the run again"))),
				ginkgo.Entry("an expectation that a stored finding does not exist", func(w *cleaning.RunWrite) { w.Upserts[0].ExpectState = "" },
					gomega.And(gomega.MatchError(cleaning.ErrStateChanged), errContaining("Preview the run again"))),
				ginkgo.Entry("an expectation for a finding that is not stored", func(w *cleaning.RunWrite) {
					w.Upserts[1].ExpectState = cleaning.StateToken(w.Upserts[1].Finding)
				}, gomega.And(gomega.MatchError(cleaning.ErrStateChanged), errContaining("Preview the run again"))),
				ginkgo.Entry("a decision on a finding not stored", func(w *cleaning.RunWrite) { w.Upserts = w.Upserts[:1] },
					errContaining("which is not stored")),
				ginkgo.Entry("a decision with an unknown verdict", func(w *cleaning.RunWrite) { w.Decisions[0].Verdict = "bogus" },
					errContaining(`unknown verdict "bogus"`)),
				ginkgo.Entry("a decision ID already stored", func(w *cleaning.RunWrite) { w.Decisions[0].ID = "d-0" },
					errContaining("already recorded")),
				ginkgo.Entry("a decision without a request", func(w *cleaning.RunWrite) { w.Decisions[0].Request = nil },
					errContaining("request and attestation")),
				ginkgo.Entry("a decision without an attestation", func(w *cleaning.RunWrite) { w.Decisions[0].Attestation = nil },
					errContaining("request and attestation")),
				ginkgo.Entry("a policy decision without a run ID", func(w *cleaning.RunWrite) { w.Decisions[0].RunID = "" },
					errContaining("run ID")),
				ginkgo.Entry("a reviewer's decision", func(w *cleaning.RunWrite) {
					w.Decisions[0].DecidedBy, w.Decisions[0].RunID = "alice", ""
				}, errContaining("record a reviewer's decision with Decide")),
				ginkgo.Entry("a reviewer's decision carrying a run ID", func(w *cleaning.RunWrite) { w.Decisions[0].DecidedBy = "alice" },
					errContaining("record a reviewer's decision with Decide")),
			)
		})

		ginkgo.Describe("Decide", func() {
			var f, next cleaning.StoredFinding

			ginkgo.BeforeEach(func() {
				f = ContractFinding(1, cleaning.SignalCrossJobContradiction, cleaning.StatusPending, "cat-x", "cat-y")
				apply(ctxA, tenA, f)
				next = f
				next.Status = cleaning.StatusRetired
				next.EdgeIDs = []string{"edge-1-a"}
			})

			ginkgo.It("records the decision byte for byte and moves the finding to next, unprojected", func() {
				d := decision("d-1", f, cleaning.VerdictRetire)
				gomega.Expect(store.Decide(ctxA, tenA, d, cleaning.StateToken(f), next)).To(gomega.Succeed())
				got, ds := get(f.ID)
				want := next
				want.GraphApplied = false
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(want))
				gomega.Expect(ds).To(gomega.Equal([]cleaning.Decision{d}))
				un, err := store.Unprojected(ctxA, tenA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ids(un)).To(gomega.Equal([]string{f.ID}))
			})

			ginkgo.It("keeps decisions oldest first", func() {
				d1 := decision("d-1", f, cleaning.VerdictRetire)
				gomega.Expect(store.Decide(ctxA, tenA, d1, cleaning.StateToken(f), next)).To(gomega.Succeed())
				restored := next
				restored.Status = cleaning.StatusRestored
				d2 := decision("d-0", f, cleaning.VerdictRestore)
				d2.DecidedAt = d1.DecidedAt.Add(time.Second)
				gomega.Expect(store.Decide(ctxA, tenA, d2, cleaning.StateToken(next), restored)).To(gomega.Succeed())
				_, ds := get(f.ID)
				gomega.Expect(ds).To(gomega.Equal([]cleaning.Decision{d1, d2}))
			})

			// mutate gets the decision, the expected state token and next as a
			// valid call would pass them, and breaks one.
			refused := func(mutate func(d *cleaning.Decision, expect *string, next *cleaning.StoredFinding)) error {
				d := decision("d-1", f, cleaning.VerdictRetire)
				expect, n := cleaning.StateToken(f), next
				mutate(&d, &expect, &n)
				err := store.Decide(ctxA, tenA, d, expect, n)
				got, ds := get(f.ID)
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(f))
				gomega.Expect(ds).To(gomega.BeEmpty())
				return err
			}
			unknown := func() cleaning.StoredFinding {
				return ContractFinding(9, cleaning.SignalLowAgreement, cleaning.StatusRetired, "cat-x", "cat-y")
			}

			ginkgo.DescribeTable("refuses the decision with a sentinel and changes nothing",
				func(mutate func(d *cleaning.Decision, expect *string, next *cleaning.StoredFinding), want error) {
					gomega.Expect(refused(mutate)).To(gomega.MatchError(want))
				},
				ginkgo.Entry("ErrStateChanged when the finding changed since it was read",
					func(_ *cleaning.Decision, expect *string, n *cleaning.StoredFinding) {
						*expect = cleaning.StateToken(*n)
					},
					cleaning.ErrStateChanged),
				ginkgo.Entry("ErrReviewerNotRegistered for an unknown reviewer",
					func(d *cleaning.Decision, _ *string, _ *cleaning.StoredFinding) { d.DecidedBy = "mallory" },
					cleaning.ErrReviewerNotRegistered),
				ginkgo.Entry("ErrReviewerDeleted for a deleted reviewer",
					func(d *cleaning.Decision, _ *string, _ *cleaning.StoredFinding) { d.DecidedBy = "gone" },
					cleaning.ErrReviewerDeleted),
				ginkgo.Entry("ErrFindingNotFound for an unknown finding",
					func(d *cleaning.Decision, _ *string, n *cleaning.StoredFinding) {
						*n = unknown()
						d.FindingID = n.ID
					},
					cleaning.ErrFindingNotFound),
			)

			ginkgo.DescribeTable("refuses the decision with an actionable message and changes nothing",
				func(mutate func(d *cleaning.Decision, expect *string, next *cleaning.StoredFinding), want gomegatypes.GomegaMatcher) {
					gomega.Expect(refused(mutate)).To(want)
				},
				ginkgo.Entry("a next that is another finding",
					func(_ *cleaning.Decision, _ *string, n *cleaning.StoredFinding) { *n = unknown() },
					errContaining("next is finding")),
				ginkgo.Entry("an unknown verdict",
					func(d *cleaning.Decision, _ *string, _ *cleaning.StoredFinding) { d.Verdict = "bogus" },
					errContaining(`unknown verdict "bogus"`)),
				ginkgo.Entry("a policy decision",
					func(d *cleaning.Decision, _ *string, _ *cleaning.StoredFinding) {
						d.DecidedBy = cleaning.DecidedByPolicy
					},
					errContaining("policy decisions are recorded only by ApplyRun")),
				ginkgo.Entry("a reviewer's decision carrying a run ID",
					func(d *cleaning.Decision, _ *string, _ *cleaning.StoredFinding) { d.RunID = "run-1" },
					errContaining("run ID")),
				ginkgo.Entry("a next with a target that has an empty key part",
					func(_ *cleaning.Decision, _ *string, n *cleaning.StoredFinding) {
						n.Targets = []cleaning.VerdictKey{{JobID: "j", SourceID: "", TargetID: "t"}}
					},
					gomega.And(gomega.MatchError(cleaning.ErrInvalidVerdictKey), errContaining("nothing was written"))),
			)
		})

		ginkgo.Describe("ListFindings and Findings", func() {
			var stored []cleaning.StoredFinding

			ginkgo.BeforeEach(func() {
				stored = nil
				for n := range 7 {
					status, signal := cleaning.StatusReported, cleaning.SignalLowAgreement
					if n%2 == 0 {
						status = cleaning.StatusPending
					}
					if n%3 == 0 {
						signal = cleaning.SignalStalePrompt
					}
					source, target := "cat-x", "cat-y"
					if n == 5 {
						source, target = "cat-y", "cat-x"
					}
					if n == 6 {
						source, target = "cat-x", "cat-z"
					}
					stored = append(stored, ContractFinding(n, signal, status, source, target))
				}
				apply(ctxA, tenA, stored...)
				slices.SortFunc(stored, func(x, y cleaning.StoredFinding) int {
					switch {
					case x.ID < y.ID:
						return -1
					case x.ID > y.ID:
						return 1
					}
					return 0
				})
			})

			list := func(f cleaning.FindingFilter) []string {
				var out []string
				for range len(stored) + 1 {
					page, next, err := store.ListFindings(ctxA, tenA, f)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(len(page)).To(gomega.BeNumerically("<=", f.PageSize))
					out = append(out, ids(page)...)
					if next == "" {
						return out
					}
					f.PageToken = next
				}
				ginkgo.Fail("ListFindings never returned an empty next-page token")
				return nil
			}
			idsWhere := func(keep func(cleaning.StoredFinding) bool) []string {
				var out []string
				for _, f := range stored {
					if keep(f) {
						out = append(out, f.ID)
					}
				}
				return out
			}

			ginkgo.DescribeTable("pages through every match exactly once, in ID order",
				func(pageSize int) {
					gomega.Expect(list(cleaning.FindingFilter{PageSize: pageSize})).To(gomega.Equal(ids(stored)))
				},
				ginkgo.Entry("one per page", 1),
				ginkgo.Entry("a page size that does not divide the count", 3),
				ginkgo.Entry("a page size equal to the count", 7),
				ginkgo.Entry("a page size above the count", 50),
			)

			ginkgo.It("returns every field of a listed finding", func() {
				page, _, err := store.ListFindings(ctxA, tenA, cleaning.FindingFilter{PageSize: 1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(page).To(gomega.HaveLen(1))
				gomega.Expect(withoutStamp(page[0])).To(gomega.Equal(stored[0]))
			})

			ginkgo.It("filters on signal", func() {
				gomega.Expect(list(cleaning.FindingFilter{Signal: cleaning.SignalStalePrompt, PageSize: 2})).To(gomega.Equal(
					idsWhere(func(f cleaning.StoredFinding) bool { return f.Signal == cleaning.SignalStalePrompt })))
			})

			ginkgo.It("filters on status", func() {
				gomega.Expect(list(cleaning.FindingFilter{Status: cleaning.StatusPending, PageSize: 2})).To(gomega.Equal(
					idsWhere(func(f cleaning.StoredFinding) bool { return f.Status == cleaning.StatusPending })))
			})

			ginkgo.It("filters on the catalog pair in either direction", func() {
				onXY := idsWhere(func(f cleaning.StoredFinding) bool { return f.TargetCatalog != "cat-z" })
				gomega.Expect(onXY).To(gomega.HaveLen(6))
				gomega.Expect(list(cleaning.FindingFilter{SourceCatalog: "cat-y", TargetCatalog: "cat-x", PageSize: 4})).To(gomega.Equal(onXY))
			})

			ginkgo.It("applies a scope to Findings in either direction", func() {
				fs, err := store.Findings(ctxA, tenA, cleaning.Scope{SourceCatalog: "cat-z", TargetCatalog: "cat-x"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ids(fs)).To(gomega.Equal(idsWhere(func(f cleaning.StoredFinding) bool { return f.TargetCatalog == "cat-z" })))
				gomega.Expect(ids(allA())).To(gomega.Equal(ids(stored)))
			})

			ginkgo.DescribeTable("refuses a filter that cannot mean what the caller wants",
				func(f cleaning.FindingFilter, msg string) {
					_, _, err := store.ListFindings(ctxA, tenA, f)
					gomega.Expect(err).To(errContaining(msg))
				},
				ginkgo.Entry("a catalog pair with one side", cleaning.FindingFilter{SourceCatalog: "cat-x", PageSize: 1}, "set both"),
				ginkgo.Entry("a zero page size", cleaning.FindingFilter{}, "page size"),
				ginkgo.Entry("a negative page size", cleaning.FindingFilter{PageSize: -1}, "page size"),
			)

			ginkgo.It("refuses a scope with one side", func() {
				_, err := store.Findings(ctxA, tenA, cleaning.Scope{TargetCatalog: "cat-x"})
				gomega.Expect(err).To(errContaining("set both"))
			})

			ginkgo.It("counts pending findings", func() {
				n, err := store.PendingCount(ctxA, tenA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(n).To(gomega.Equal(int64(len(idsWhere(func(f cleaning.StoredFinding) bool { return f.Status == cleaning.StatusPending })))))
			})
		})

		ginkgo.Describe("Unprojected and MarkProjected", func() {
			ginkgo.It("round-trip: a projected finding leaves the list with its new edges", func() {
				applied := ContractFinding(1, cleaning.SignalLowAgreement, cleaning.StatusReported, "cat-x", "cat-y")
				waiting := ContractFinding(2, cleaning.SignalLowAgreement, cleaning.StatusRetired, "cat-x", "cat-y")
				waiting.GraphApplied = false
				apply(ctxA, tenA, applied, waiting)

				un, err := store.Unprojected(ctxA, tenA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(un).To(gomega.HaveLen(1))
				gomega.Expect(withoutStamp(un[0])).To(gomega.Equal(waiting))

				gomega.Expect(store.MarkProjected(ctxA, tenA, waiting.ID, []string{"edge-new"})).To(gomega.Succeed())
				un, err = store.Unprojected(ctxA, tenA)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(un).To(gomega.BeEmpty())
				got, _ := get(waiting.ID)
				want := waiting
				want.GraphApplied, want.EdgeIDs = true, []string{"edge-new"}
				gomega.Expect(withoutStamp(got)).To(gomega.Equal(want))
			})

			ginkgo.It("refuses to mark an unknown finding", func() {
				gomega.Expect(store.MarkProjected(ctxA, tenA, "unknown", nil)).To(gomega.MatchError(cleaning.ErrFindingNotFound))
			})
		})
	})
}
