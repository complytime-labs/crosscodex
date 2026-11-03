// Package resultcachetest provides a test double for code that consumes
// resultcache.Store.
//
// # Fakes
//
// Fake fakes resultcache.Store as a stateful in-memory store that follows
// the semantics of pkg/resultcache/pgstore and of the llm_result_cache,
// job_cache_entries and job_cache_stats tables (migration
// 001_initial_schema):
//
//   - Tenant scope. Every method returns an error wrapping
//     db.ErrTenantRequired when ctx carries no tenant, before it touches
//     state or runs a Func. Every read and write is the ctx tenant's only.
//   - Parents. Put refuses a job or catalog that was not added for the ctx
//     tenant (AddJob, AddCatalog), wrapping ErrUnknownParent, where Postgres
//     refuses the row with a foreign-key violation. RecordStage and
//     Checkpoint refuse such a job wrapping resultcache.ErrUnknownJob, as
//     pgstore does. Validation (resultcache.Entry.Validate,
//     StageRecord.Validate) runs first, as pgstore runs it.
//   - Frozen records. AddJob adds a running job; SetJobStatus moves it to
//     another status. Put and RecordStage refuse a job whose status is not
//     "running", wrapping resultcache.ErrJobNotRunning with the wording of
//     the require_running_job_for_cache_record trigger's message, and
//     change nothing.
//   - Put replaces the row for (tenant, analyzer, key) and adds the job's
//     write entry, together. RecordStage adds hit entries and appends a
//     stats row, together, under the record's attempt. Entries are a set
//     per attempt: recording a fact twice in one attempt keeps one, in two
//     attempts keeps both.
//   - Producers returns each stored row's producer. LookupVerifiable builds
//     a resultcache.Snapshot of the rows and the requested attempts of their
//     producers (each once), read under one hold of the fake's lock (its
//     snapshot); it refuses an attempt for another analyzer. Manifest covers
//     one attempt (refusing an invalid one with resultcache.ErrInvalidEntry)
//     and returns entries in resultcache.CompareEntries order and stats
//     oldest first. VerifiedSummaries reads each signed attempt and returns
//     resultcache.SummarizeSigned of them.
//   - Job locks. Checkpoint holds a per-job lock exclusively while it
//     compares the record (resultcache.MatchRecord) and runs seal, and Put
//     and RecordStage take it shared, as FOR UPDATE and the freeze
//     trigger's FOR SHARE do; seal must not call the fake. LookupVerifiable
//     takes no job lock, so it does not wait on a Checkpoint in progress.
//     That is harmless: a checkpoint writes no cache rows, so the snapshot
//     is the same whether it is read before, during or after one.
//
// StoreContract registers the specs that define the behaviors above which
// Postgres also exhibits. This package's suite runs it against Fake and
// pkg/resultcache/pgstore's integration suite runs it against pgstore as
// app_user, so the two cannot drift. Anything Postgres does differently
// from the contract is a bug in Fake.
//
// Gaps: ErrUnknownParent's text is not Postgres's foreign-key error; the
// contract asserts only that an error occurs. The Fake has no created_at
// and no seq.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test
// package uses, except that every method has a natural default (the
// stateful behavior above), so there is no ErrUnscripted:
//
//   - A method whose result a spec must force is scripted by a <Method>Func
//     field with the method's exact signature (ProducersFunc, PutFunc,
//     RecordStageFunc, ManifestFunc, LookupVerifiableFunc, CheckpointFunc,
//     VerifiedSummariesFunc). When set, it alone decides the result and
//     state is left untouched. The tenant check still runs first.
//   - State is read through the Store methods themselves; returned results
//     and maps are copies.
//   - The zero value is ready to use. Set the exported fields before the
//     first call; method calls (AddJob, AddCatalog and SetJobStatus
//     included) are safe for concurrent use, writes to the exported fields
//     are not.
//   - A Func cannot call the default behavior. To intercept a few methods
//     and keep the default for the rest, embed *Fake and override them.
//
// Production code must not import this package.
package resultcachetest
