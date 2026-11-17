// Package cleaningtest provides a test double for code that consumes
// cleaning.FindingStore.
//
// # Fakes
//
// Fake fakes cleaning.FindingStore as a stateful in-memory store that follows
// the semantics of the Postgres-backed store (cleaning.NewPGFindingStore) and
// of the edge_cleaning_runs, edge_findings and edge_finding_decisions tables
// it writes (migration 001_initial_schema):
//
//   - Tenant scope. The real store runs every call in a tenant-scoped
//     transaction. Every Fake method returns an error wrapping
//     tenant.ErrNoTenant when ctx carries no tenant, and an error saying the
//     tenants do not match when the ctx tenant differs from tenantID, before
//     it touches state or runs a Func.
//   - Row-level security. Runs, findings and decisions are kept per tenant; one
//     tenant's finding ID is ErrFindingNotFound to another, and the same ID
//     may exist in both.
//   - ApplyRun refuses, before touching state, any write
//     cleaning.RunWrite.Validate refuses. It then records the run, refusing
//     one whose PlanToken and AppliedAt are already stored (primary key);
//     Fake.Runs reads the records back. It writes each upsert whose ExpectState holds ("" when the finding is not
//     stored, else its StateToken; otherwise ErrStateChanged), stamping
//     UpdatedAt from Fake.Now, and appends each decision, refusing a finding
//     not stored (foreign key) or an ID already stored in the tenant
//     (primary key). It is all or nothing.
//   - Decide refuses a next whose ID is not the decision's finding, a policy
//     decision, and a decision or next the validators refuse; then checks
//     the reviewer in Fake.Reviewers (absent: ErrReviewerNotRegistered;
//     ReviewerDeleted: ErrReviewerDeleted), the finding (absent:
//     ErrFindingNotFound) and its state token (different: ErrStateChanged),
//     then the decision's ID as ApplyRun does. On success the finding takes
//     next's Status, Targets and EdgeIDs with GraphApplied false.
//   - Refusals the real store words itself (a stale run, a run already
//     recorded, a decision on a
//     finding not stored or already recorded, a non-policy decision in a
//     run, a policy decision in Decide) use the same text in the Fake.
//   - Times are stored as the database stores them, in UTC. A time the
//     caller passes (Decision.DecidedAt, RunRecord.AppliedAt) is truncated
//     to the microsecond, as the pgx driver encodes it; a zero
//     Decision.DecidedAt becomes Fake.Now, rounded to the microsecond as the
//     database's own clock is.
//   - Findings and ListFindings apply cleaning.Scope's unordered catalog-pair
//     rule and refuse a pair with one side set; ListFindings refuses a page
//     size below 1 and pages by ID (the next-page token is the last ID of a
//     full page). Results are sorted by ID; decisions oldest DecidedAt first,
//     then by ID.
//   - Unprojected returns findings with GraphApplied false; MarkProjected sets
//     it and replaces EdgeIDs, refusing edge IDs over the stored-JSON bound.
//     PendingCount counts status pending.
//   - CompletedJobs returns Fake.Jobs[tenantID] sorted by ID. The Fake holds
//     jobs already parsed, so it never fails on a job.
//
// FindingStoreContract registers the specs that define the behaviors above
// which Postgres also exhibits. cleaningtest's own suite runs it against Fake
// and pkg/cleaning/store_integration_bdd_test.go runs it against the Postgres
// store as the restricted role, so the two cannot drift. Anything Postgres
// does differently from the contract is a bug in Fake.
//
// Not covered by the contract: CompletedJobs (its parsing of jobs.config and
// job_stages.stage_record is specified by pkg/cleaning's own specs), Seed,
// Jobs, Now, the Funcs and copy semantics.
//
// Gaps: the Fake's reviewer and tenant errors do not name the admin
// commands the real store's do, so no spec should match on their text beyond
// the sentinels. Fake.Reviewers does not model a deleted tenant, which the real
// reviewer check also refuses.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses, except that every method has a natural default (the stateful
// behavior above), so there is no ErrUnscripted:
//
//   - A method whose result a spec must force (usually an injected error) is
//     scripted by a <Method>Func field with the method's exact signature
//     (Fake.ApplyRunFunc, Fake.MarkProjectedFunc, ...). When set, it alone
//     decides the result and the store is left untouched. The tenant check
//     still runs first.
//   - Fake.Seed preloads findings per tenant as stored, bypassing the checks
//     above, including UpdatedAt. It is read once, on first use.
//   - Fake.Reviewers maps a tenant to reviewer IDs and their status,
//     ReviewerActive or ReviewerDeleted; any other status fails Decide.
//   - State is read through the FindingStore methods themselves; the Fake
//     has no separate recorder.
//   - The zero value is ready to use. Set the exported fields before the first
//     call: method calls are safe for concurrent use, writes to the exported
//     fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package cleaningtest
