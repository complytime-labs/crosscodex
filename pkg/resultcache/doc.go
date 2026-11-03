// Package resultcache reuses LLM results across one tenant's analysis jobs
// (#243).
//
// A cache key names one exact LLM request. Key hashes a length-prefixed
// canonical encoding of the analyzer name, the request's Identity (task
// type, model alias, decoding parameters, sample index and the request's
// content hash, which covers the rendered messages or the embedded text),
// the model the gateway reported for the alias, and the operator-declared
// revision. Every backend shares Key, so a key the engine computes finds the
// row any backend stored.
//
// Store is the storage contract. pkg/resultcache/pgstore implements it on
// PostgreSQL; pkg/resultcache/resultcachetest provides the shared fake and
// StoreContract, which binds both. Every Store call is scoped to the tenant
// in ctx (tenant.FromContext); one tenant never sees another's rows.
//
// A stored result is the worker's result struct as EncodeResult encodes it:
// encoding/json over the struct's map, so keys are sorted and equal results
// encode to equal bytes. Every stage run records under its own attempt ID,
// and when it stops the executor checkpoints it: Store.Checkpoint compares
// the executor's in-process record with the attempt's rows under the job
// lock and, only if they match, lets it sign the record's Manifest.Digest.
// Trust comes only from those checkpoints: Store.LookupVerifiable reads the
// cached rows and their producers' checkpointed manifests in one snapshot,
// and Snapshot.Verify, the only way to read a cached result, accepts it
// only if each signed manifest still matches and one of them vouches for
// its bytes (a write entry, or a self-hit). Store.VerifiedSummaries shows
// a checkpointed attempt only while its rows match. Rows of unsigned
// attempts never count. DecodeResult validates the bytes at the
// deserialization point.
package resultcache
