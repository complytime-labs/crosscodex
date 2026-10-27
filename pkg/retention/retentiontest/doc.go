// Package retentiontest provides a test double for code that consumes
// retention.HoldStore.
//
// # Fakes
//
// Fake fakes retention.HoldStore as a stateful in-memory store that follows
// the semantics of the Postgres-backed store (retention.NewPgHoldStore) and of
// the retention_holds table it writes (migration 001_initial_schema):
//
//   - Tenant scope. The real store runs every call in a tenant-scoped
//     transaction, which needs a tenant in ctx. Every Fake method returns an
//     error wrapping db.ErrTenantRequired when ctx carries none, before it
//     touches state or runs a Func.
//   - Row-level security. A hold is visible to a call when its Scope.TenantID
//     is the ctx tenant or empty (a global hold). List and Active return only
//     visible holds; Release only touches visible ones. Create refuses a hold
//     whose Scope.TenantID is not the ctx tenant, global holds included (the
//     table's WITH CHECK clause), and Release returns an error for a visible
//     global hold for the same reason. Global holds therefore exist only
//     through Fake.Seed, as they do in production (a privileged path).
//   - Create returns the hold as the database would store it: Fake-assigned ID
//     ("hold-1", "hold-2", ...), CreatedAt from Fake.Now, the caller's name,
//     scope, CreatedBy and ExpiresAt, and no release data. Caller-supplied ID,
//     CreatedAt, ReleasedAt and ReleasedBy are ignored, as the INSERT ignores
//     them. A second Create of a name that has an unreleased hold fails (the
//     table's partial unique index), even when that hold belongs to another
//     tenant.
//   - Release marks every visible unreleased hold with the name as released
//     at Fake.Now by releasedBy. An unknown name, an already released hold and
//     another tenant's hold are all a silent no-op, as in production.
//   - List returns visible holds, released ones included. Active returns the
//     visible holds for which retention.Hold.ActiveAt(now) is true. Both order
//     newest CreatedAt first; holds with equal CreatedAt come back most
//     recently stored first (production leaves that order undefined).
//
// HoldStoreContract registers the specs that define the behaviors above which
// Postgres also exhibits: tenant required (db.ErrTenantRequired) with nothing
// changed; row-level-security visibility; Create refusing another tenant's or
// a global scope; the unique unreleased name, across tenants; Create ignoring
// caller-supplied ID, CreatedAt and release data; Release marking the hold,
// being a silent no-op for unknown, released and foreign holds, and failing
// for a visible global hold; Active dropping released and expired holds (a
// hold expiring exactly at now is expired); newest-CreatedAt-first ordering.
// retentiontest's own suite runs it against Fake and
// pkg/retention/hold_store_integration_bdd_test.go runs it against the
// Postgres store as the restricted role, so the two cannot drift. Anything
// Postgres does differently from the contract is a bug in Fake.
//
// Not covered by the contract, because Postgres does not do it: the ID format,
// the order of equal CreatedAt values (undefined in production), Now, Seed,
// the Funcs, and copy semantics.
//
// Gaps: the errors Fake returns for a refused Create (scope tenant, duplicate
// name) and a refused Release (global hold) are short cause-only text. Production
// returns the raw driver error, wrapped as "hold store: create: ..." or
// "hold store: release: ...", so no spec should match on the text; the contract
// asserts only that an error occurs. IDs are not UUIDs. The Fake keeps no
// audit trail beyond the holds.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses, except that every method has a natural default (the stateful
// behavior above), so there is no ErrUnscripted:
//
//   - A method whose result a spec must force (usually an injected error) is
//     scripted by a <Method>Func field with the method's exact signature
//     (Fake.CreateFunc, Fake.ReleaseFunc, Fake.ListFunc, Fake.ActiveFunc).
//     When set, it alone decides the result and the stored holds are left
//     untouched. The tenant check still runs first.
//   - Fake.Seed preloads holds as stored, bypassing the checks above: any
//     tenant (or none), any ID and timestamps. It is read once, on first use.
//     Generated IDs skip any ID a stored hold already has.
//   - State is read through the HoldStore methods themselves (List with a
//     tenant ctx); the Fake has no separate recorder.
//   - The zero value is ready to use. Set the exported fields before the first
//     call: method calls are safe for concurrent use, writes to the exported
//     fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package retentiontest
