// Package vectordbtest provides a test double for code that consumes
// vectordb.VectorDB.
//
// # Fakes
//
// Fake fakes vectordb.VectorDB. Before it records a call or runs a scripted
// Func it applies the boundary checks the real PgVectorStore applies ahead of
// any SQL, so a spec cannot pass on a call production would refuse:
//
//   - Every method requires a tenant in ctx (tenant.FromContext) that equals
//     the tenant argument; otherwise it returns an error wrapping
//     tenant.ErrNoTenant or tenant.ErrTenantMismatch. Production checks the
//     context tenant against the argument and does not itself call
//     tenant.ValidateTenantID, so neither does the Fake.
//   - FindSimilar rejects a Limit that is not positive and an empty query
//     Vector, with the same messages as production. Production exposes no
//     sentinel for these, so a spec can only match the message text.
//   - StoreBatch accepts an empty batch, as production does (a no-op there).
//     The Fake still records it, so a spec that asserts "nothing was stored"
//     with Stored cannot pass vacuously when code stores an empty batch.
//
// Rejected calls are not recorded and never reach a Func.
//
// A spec asserting that code under test must not store (or must not search)
// needs inputs the Fake would accept (a tenant-scoped context, a valid query),
// or the assertion passes vacuously because the Fake refused the call itself.
//
// Gaps: the error message text of the tenant checks (the "tenant context
// required" and "tenant mismatch" wording) and of the FindSimilar query checks
// is copied from unexported production validation and can drift; the
// semantics (which calls are refused, tenant.ErrNoTenant,
// tenant.ErrTenantMismatch) come from exported sentinels and the documented
// contract. A Fake does no model, dimension or catalog-ownership checking
// (those are SQL-side in production), holds no embeddings, and so cannot
// answer FindSimilar by itself; it emits no telemetry. Stored copies the
// Metadata map one level deep: the values inside it are shared with the
// caller.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - Every method is scripted by a <Method>Func field with the method's
//     exact signature (Fake.StoreEmbeddingFunc, Fake.StoreBatchFunc,
//     Fake.FindSimilarFunc, Fake.DeleteByModelFunc). When set, it alone
//     decides the result; an accepted store call is recorded either way.
//   - StoreEmbedding and StoreBatch have a natural default: with no Func they
//     record the call and return nil, as a store that succeeds. FindSimilar
//     and DeleteByModel have none, so with no Func they return an error
//     wrapping ErrUnscripted that names the field to set. A fake never
//     panics.
//   - A Fake records only what some spec asserts on, through a method that
//     returns a copy (see Gaps for Metadata) (Fake.Stored: the tenant and
//     embeddings of every accepted StoreEmbedding or StoreBatch call, in call
//     order; a StoreEmbedding call is one entry with one embedding).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package vectordbtest
