package resultcache

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"
)

var (
	// ErrUnavailable is the class sentinel of a stage stopped because the
	// cache could not be read, its audit rows written or its record sealed;
	// the job error class is result_cache_unavailable (retryable). Only the
	// engine sets it (internal/analysis StageError.Sentinel); Store
	// implementations do not wrap it, and the engine classifies their
	// errors by the call that failed.
	ErrUnavailable = errors.New("result cache unavailable")
	// ErrCorrupt reports a cached result that failed its integrity check or
	// does not decode. The result is never reused; the job error class is
	// result_cache_corrupt (not retryable).
	ErrCorrupt = errors.New("cached result failed its integrity check")
	// ErrInvalidEntry reports a Store argument that breaks a table rule.
	// Every backend refuses it before writing anything.
	ErrInvalidEntry = errors.New("invalid result cache entry")
	// ErrJobNotRunning reports a write entry, hit entry or stats row for a
	// job whose status is not "running": a job's cache record is frozen
	// once it stops (the require_running_job_for_cache_record trigger).
	ErrJobNotRunning = errors.New("result cache record refused: the job is not running")
	// ErrUntrusted reports a cached result no checkpoint-verified attempt
	// can vouch for, because there is no row or no signed attempt of its
	// producer (crashed, unsigned, or signed by a key no longer trusted).
	// It is a miss: recompute the result.
	ErrUntrusted = errors.New("cached result is not trusted")
	// ErrUnknownJob reports a job the tenant does not have, or cannot see.
	ErrUnknownJob = errors.New("no such job in this tenant")
)

// ProbePromptName is the prompt name of the per-job model probe's
// completion request (internal/pipeline), so the LLM audit trail and test
// doubles can tell a probe from an analysis call.
const ProbePromptName = "result-cache-probe"

// keyVersion leads every key encoding, so a change to the encoding yields
// new keys instead of colliding with keys of the old one.
const keyVersion = "crosscodex.resultcache.v1"

// Identity is what makes an LLM request that request, apart from the model
// the gateway resolves its alias to. It holds no job-specific value (job
// ID, task ID), so the same request in two jobs has the same identity.
//
// Every field of llmclient.CompletionRequest and EmbeddingRequest that
// changes the model's output must be part of the identity (directly, or
// through ContentHash); resultcache_bdd_test.go pins the current field
// list, so adding a request field forces that decision.
type Identity struct {
	// TaskType is the analyzer that built the request.
	TaskType string
	// ModelAlias is the model the request names, as the task payload carries
	// it: "" for the tenant default the worker substitutes.
	ModelAlias string
	// Temperature is the sampling temperature sent, or nil when the request
	// sends none (embeddings).
	Temperature *float64
	// MaxTokens is the completion limit sent; 0 when the request sends none.
	MaxTokens int64
	// Sample is the sample or voter index. Requests that differ only here
	// must not share a key: one response replayed N times would corrupt a
	// consensus.
	Sample int64
	// ContentHash is the payload's content_hash: of the rendered messages
	// for a completion, of the text for an embedding.
	ContentHash string
}

// Key returns the cache key of one LLM request: the lowercase hex SHA-256
// of its canonical encoding. observedModel is the model the gateway
// reported (the probe's resp.Model for a lookup, the producing call's for a
// put); revision is llm.model_revisions for the alias, or "".
func Key(analyzerName string, id Identity, observedModel, revision string) string {
	sum := sha256.Sum256(encodeKey(analyzerName, id, observedModel, revision))
	return hex.EncodeToString(sum[:])
}

// encodeKey is the canonical encoding Key hashes: keyVersion, then every
// field as an 8-byte big-endian length followed by its bytes, in a fixed
// order. The length prefixes make it injective, so two different requests
// share a key only if SHA-256 collides. A nil temperature encodes as the
// empty string, which strconv.FormatFloat never returns.
func encodeKey(analyzerName string, id Identity, observedModel, revision string) []byte {
	temperature := ""
	if id.Temperature != nil {
		temperature = strconv.FormatFloat(*id.Temperature, 'g', -1, 64)
	}
	var buf bytes.Buffer
	for _, field := range []string{
		keyVersion, analyzerName, id.TaskType, id.ModelAlias, temperature,
		strconv.FormatInt(id.MaxTokens, 10), strconv.FormatInt(id.Sample, 10),
		id.ContentHash, observedModel, revision,
	} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		buf.Write(n[:])
		buf.WriteString(field)
	}
	return buf.Bytes()
}

// Mode is how one analyzer of a job used the cache.
type Mode string

const (
	// ModeEnabled: requests were looked up, hits reused, misses stored.
	ModeEnabled Mode = "enabled"
	// ModeBypassed: the job's no_cache; nothing was looked up, every result
	// was stored.
	ModeBypassed Mode = "bypassed"
	// ModeDisabled: analysis.result_cache.enabled is false for the tenant;
	// nothing was looked up or stored.
	ModeDisabled Mode = "disabled"
	// ModeSkipped: the analyzer was not cached; Stats.Reason says why.
	ModeSkipped Mode = "skipped"
)

// Valid reports whether m is one of the four modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeEnabled, ModeBypassed, ModeDisabled, ModeSkipped:
		return true
	default:
		return false
	}
}

// ModelIdentity is the concrete model behind one alias in one job.
type ModelIdentity struct {
	// Observed is the probe's resp.Model; empty when the gateway returned
	// none, and the alias was then not cached in the job.
	Observed string `json:"observed"`
	// Revision is llm.model_revisions for the alias, or "".
	Revision string `json:"revision"`
}

// Roles of a job_cache_entries row.
const (
	// RoleHit: the job reused the result another job (the producer) stored.
	RoleHit = "hit"
	// RoleWrite: the job stored the result.
	RoleWrite = "write"
)

// Entry is one result a job stores. Store.Put writes the cache row and the
// job's write entry for it in one transaction.
type Entry struct {
	JobID    string
	Analyzer string
	// Attempt is the stage run that stored the result (ValidAttempt).
	Attempt  string
	TaskID   string
	CacheKey string
	// Result is EncodeResult's encoding of the worker's result struct.
	Result []byte
	// ResponseModel is resp.Model of the call that produced Result. The
	// engine stores a result only when it is the alias's observed model, the
	// one CacheKey was built with.
	ResponseModel string
	// SourceCatalogID and TargetCatalogID are the catalogs of the controls
	// whose text the request carried (equal for a per-control task), so the
	// row is purged with either catalog.
	SourceCatalogID string
	TargetCatalogID string
}

// Validate returns an error wrapping ErrInvalidEntry naming every field
// that breaks a table rule, or nil.
func (e Entry) Validate() error {
	var p []string
	for _, f := range []struct{ name, value string }{
		{"job_id", e.JobID}, {"analyzer", e.Analyzer}, {"task_id", e.TaskID}, {"response_model", e.ResponseModel},
		{"source_catalog_id", e.SourceCatalogID}, {"target_catalog_id", e.TargetCatalogID},
	} {
		if f.value == "" {
			p = append(p, f.name+" is empty")
		}
	}
	p = append(p, attemptProblems(e.Attempt)...)
	if !validKey(e.CacheKey) {
		p = append(p, fmt.Sprintf("cache_key %q is not 64 lowercase hex characters", e.CacheKey))
	}
	switch {
	case len(e.Result) == 0:
		p = append(p, "result is empty")
	case !utf8.Valid(e.Result) || !json.Valid(e.Result):
		// The column is TEXT: invalid UTF-8 or a raw NUL would otherwise
		// fail as a driver error instead of ErrInvalidEntry.
		p = append(p, "result is not valid JSON in UTF-8")
	}
	return invalid(p)
}

// HitEntry is one result a job reused.
type HitEntry struct {
	TaskID        string
	CacheKey      string
	ResultDigest  string
	ProducerJobID string
}

// Stats is one completed stage run of one analyzer of a job.
type Stats struct {
	Mode   Mode   `json:"mode"`
	Reason string `json:"reason"`
	// Models maps each alias the analyzer used, as the gateway resolves it,
	// to the model observed for it in this job.
	Models     map[string]ModelIdentity `json:"models"`
	Tasks      int                      `json:"tasks"`
	Hits       int                      `json:"hits"`
	Dispatched int                      `json:"dispatched"`
}

// Validate returns an error wrapping ErrInvalidEntry naming every rule s
// breaks, or nil.
func (s Stats) Validate() error {
	return invalid(statsProblems(s))
}

func statsProblems(s Stats) []string {
	var p []string
	if !s.Mode.Valid() {
		p = append(p, fmt.Sprintf("mode %q is not enabled, bypassed, disabled or skipped", s.Mode))
	}
	if s.Tasks < 0 || s.Hits < 0 || s.Dispatched < 0 {
		p = append(p, "tasks, hits and dispatched must not be negative")
	}
	if s.Tasks > math.MaxInt32 || s.Hits > math.MaxInt32 || s.Dispatched > math.MaxInt32 {
		p = append(p, fmt.Sprintf("tasks, hits and dispatched must not exceed %d (the columns are INTEGER)", math.MaxInt32))
	}
	if s.Mode != ModeEnabled && s.Hits != 0 {
		p = append(p, fmt.Sprintf("hits %d recorded in mode %q: only mode enabled looks results up", s.Hits, s.Mode))
	}
	if s.Hits+s.Dispatched > s.Tasks {
		p = append(p, fmt.Sprintf("hits %d plus dispatched %d exceed tasks %d", s.Hits, s.Dispatched, s.Tasks))
	}
	return p
}

// StageRecord is what a job records when one analyzer's stage completes:
// its hit entries and its stats. Store.RecordStage writes both in one
// transaction, under Attempt: one stage run records once, so the attempt
// belongs to the record, not to each hit.
type StageRecord struct {
	JobID    string
	Analyzer string
	Attempt  string
	Hits     []HitEntry
	Stats    Stats
}

// Validate returns an error wrapping ErrInvalidEntry naming every rule r
// breaks, or nil.
func (r StageRecord) Validate() error {
	var p []string
	if r.JobID == "" {
		p = append(p, "job_id is empty")
	}
	if r.Analyzer == "" {
		p = append(p, "analyzer is empty")
	}
	p = append(p, attemptProblems(r.Attempt)...)
	p = append(p, statsProblems(r.Stats)...)
	if len(r.Hits) != r.Stats.Hits {
		p = append(p, fmt.Sprintf("%d hit entries but stats.hits is %d", len(r.Hits), r.Stats.Hits))
	}
	seen := make(map[HitEntry]int, len(r.Hits))
	for i, h := range r.Hits {
		if first, dup := seen[h]; dup {
			p = append(p, fmt.Sprintf("hit %d duplicates hit %d, which storage would collapse into one", i, first))
		}
		seen[h] = i
		if h.TaskID == "" {
			p = append(p, fmt.Sprintf("hit %d: task_id is empty", i))
		}
		if !validKey(h.CacheKey) {
			p = append(p, fmt.Sprintf("hit %d: cache_key %q is not 64 lowercase hex characters", i, h.CacheKey))
		}
		if !validDigest(h.ResultDigest) {
			p = append(p, fmt.Sprintf("hit %d: result_digest %q is not sha256:<64 lowercase hex>", i, h.ResultDigest))
		}
		if h.ProducerJobID == "" {
			p = append(p, fmt.Sprintf("hit %d: producer_job_id is empty", i))
		}
	}
	return invalid(p)
}

// Hit is one cached result, as only Snapshot.Verify returns it.
type Hit struct {
	CacheKey      string
	Result        []byte
	ResponseModel string
	// ProducerJobID is the job that stored Result (llm_result_cache.job_id).
	ProducerJobID string
	// ProducerAttempt is the attempt (stage run) of ProducerJobID that
	// stored Result (llm_result_cache.producer_attempt). Only that
	// attempt's signed record can vouch for it.
	ProducerAttempt string
}

// AttemptRef names one attempt (stage run) of one job's analyzer.
type AttemptRef struct {
	JobID    string
	Analyzer string
	Attempt  string
}

// Snapshot is what Store.LookupVerifiable read in one REPEATABLE READ
// transaction: the cached rows of one analyzer, and the requested attempts
// of their producers. Its contents are unexported: Verify is the only way
// to read a result, and it reads nothing but the snapshot.
type Snapshot struct {
	analyzer  string
	hits      map[string]Hit
	manifests map[AttemptRef]Manifest
}

// NewSnapshot is how a Store builds the Snapshot of analyzer from the rows
// it read: hits by key, and every requested attempt of a producer of hits,
// empty when the attempt recorded nothing. Only Store implementations call
// it.
func NewSnapshot(analyzer string, hits map[string]Hit, manifests map[AttemptRef]Manifest) Snapshot {
	return Snapshot{analyzer: analyzer, hits: maps.Clone(hits), manifests: maps.Clone(manifests)}
}

// Keys returns the keys the snapshot holds a cached row for, sorted.
func (s Snapshot) Keys() []string {
	return slices.Sorted(maps.Keys(s.hits))
}

// Verify returns the hit for key when the snapshot proves it. signed maps
// each attempt whose cache checkpoint the caller verified under a trusted
// key to the manifest digest that checkpoint signed. Only the attempt that
// stored the row (its producer and producer attempt) can vouch for it:
// when signed holds it, its manifest in the snapshot must match its signed
// digest and hold a write of the result's digest under key, else an error
// wrapping ErrCorrupt (a record changed after signing, or an edited
// result). No row for key, or a storing attempt signed holds no checkpoint
// for (crashed, unsealed, or signed by a key no longer trusted), wraps
// ErrUntrusted: treat it as a miss. A signed attempt of the producer for
// another analyzer, or a storing attempt the snapshot did not read, wraps
// ErrInvalidEntry.
func (s Snapshot) Verify(key string, signed map[AttemptRef]string) (Hit, error) {
	h, ok := s.hits[key]
	if !ok {
		return Hit{}, fmt.Errorf("%w: no cached result for key %s", ErrUntrusted, key)
	}
	for ref := range signed {
		if ref.JobID == h.ProducerJobID && ref.Analyzer != s.analyzer {
			return Hit{}, fmt.Errorf("%w: attempt %s of job %s is for analyzer %q, not %q, which this snapshot read",
				ErrInvalidEntry, ref.Attempt, ref.JobID, ref.Analyzer, s.analyzer)
		}
	}
	ref := AttemptRef{JobID: h.ProducerJobID, Analyzer: s.analyzer, Attempt: h.ProducerAttempt}
	digest, ok := signed[ref]
	if !ok {
		return Hit{}, fmt.Errorf("%w: key %s: attempt %s of job %s, which stored the result, has no checkpoint-signed record",
			ErrUntrusted, key, h.ProducerAttempt, h.ProducerJobID)
	}
	m, read := s.manifests[ref]
	if !read {
		return Hit{}, fmt.Errorf("%w: attempt %s of job %s was not read in this snapshot; pass it to LookupVerifiable",
			ErrInvalidEntry, ref.Attempt, ref.JobID)
	}
	if d := m.Digest(); d != digest {
		return Hit{}, fmt.Errorf("%w: key %s: attempt %s of job %s does not match its signed checkpoint: recorded rows digest to %s, the checkpoint signed %s",
			ErrCorrupt, key, ref.Attempt, ref.JobID, d, digest)
	}
	got := ResultDigest(h.Result)
	if !slices.Contains(m.written(key), got) {
		return Hit{}, fmt.Errorf("%w: key %s: the stored result's digest %s: attempt %s of job %s does not vouch for it",
			ErrCorrupt, key, got, ref.Attempt, ref.JobID)
	}
	h.Result = slices.Clone(h.Result)
	return h, nil
}

// ResultDigest is the "sha256:<hex>" digest of a stored result, as entries
// record it.
func ResultDigest(result []byte) string {
	sum := sha256.Sum256(result)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// EncodeResult encodes a worker result for storage: encoding/json over the
// struct's map, so keys are sorted and floats use the shortest form that
// round-trips. Equal results encode to equal bytes. It refuses, wrapping
// ErrInvalidEntry, a nil result or one that names no model, which
// DecodeResult would reject as corrupt.
func EncodeResult(s *structpb.Struct) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: no result to encode", ErrInvalidEntry)
	}
	if model, _ := s.GetFields()["model"].AsInterface().(string); model == "" {
		return nil, fmt.Errorf("%w: the result names no model, so DecodeResult would refuse it", ErrInvalidEntry)
	}
	data, err := json.Marshal(s.AsMap())
	if err != nil {
		return nil, fmt.Errorf("encoding the result: %w", err)
	}
	return data, nil
}

// DecodeResult validates and decodes stored result bytes: exactly one JSON
// object naming a non-empty "model". Every refusal wraps ErrCorrupt.
func DecodeResult(data []byte) (*structpb.Struct, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("%w: the stored result is not a JSON object", ErrCorrupt)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the stored result has trailing data after its JSON object", ErrCorrupt)
	}
	if model, _ := m["model"].(string); model == "" {
		return nil, fmt.Errorf("%w: the stored result names no model", ErrCorrupt)
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("%w: the stored result is not a result struct: %w", ErrCorrupt, err)
	}
	return s, nil
}

// ManifestEntry is one job_cache_entries row of a job and analyzer.
type ManifestEntry struct {
	TaskID        string `json:"task_id"`
	Role          string `json:"role"`
	CacheKey      string `json:"cache_key"`
	ResultDigest  string `json:"result_digest"`
	ProducerJobID string `json:"producer_job_id"`
}

// CompareEntries orders manifest entries by task, role, key, digest and
// producer: the canonical order Manifest.Digest sorts by.
func CompareEntries(a, b ManifestEntry) int {
	return cmp.Or(
		strings.Compare(a.TaskID, b.TaskID),
		strings.Compare(a.Role, b.Role),
		strings.Compare(a.CacheKey, b.CacheKey),
		strings.Compare(a.ResultDigest, b.ResultDigest),
		strings.Compare(a.ProducerJobID, b.ProducerJobID),
	)
}

// Manifest is everything one attempt of one job's analyzer recorded about
// its cache use: its entries and its stats rows, oldest first. TenantID,
// JobID, Analyzer and Attempt name it and are bound into its digest.
type Manifest struct {
	TenantID string
	JobID    string
	Analyzer string
	Attempt  string
	Entries  []ManifestEntry
	Stats    []Stats
}

// Digest is the "sha256:<hex>" digest a cache checkpoint signs and the
// analyzer's attestation link pins as its <analyzer>.cache_manifest
// material: of the JSON of the tenant, job, analyzer, attempt, the entries
// in CompareEntries order and the stats in recorded order, with a nil
// Models map encoded as an empty one, so every backend reproduces it.
func (m Manifest) Digest() string {
	data, err := json.Marshal(struct {
		TenantID string          `json:"tenant_id"`
		JobID    string          `json:"job_id"`
		Analyzer string          `json:"analyzer"`
		Attempt  string          `json:"attempt"`
		Entries  []ManifestEntry `json:"entries"`
		Stats    []Stats         `json:"stats"`
	}{m.TenantID, m.JobID, m.Analyzer, m.Attempt, canonicalEntries(m.Entries), canonicalStats(m.Stats)})
	if err != nil {
		// Strings, ints and a string-keyed map of string pairs always
		// marshal; a failure is a programming error, as in
		// llmclient.ContentHash.
		panic(fmt.Sprintf("resultcache: Manifest.Digest: json.Marshal failed: %v", err))
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalEntries returns entries sorted by CompareEntries, never nil.
func canonicalEntries(entries []ManifestEntry) []ManifestEntry {
	out := slices.Clone(entries)
	if out == nil {
		out = []ManifestEntry{}
	}
	slices.SortFunc(out, CompareEntries)
	return out
}

// canonicalStats returns stats with every nil Models map made empty.
func canonicalStats(stats []Stats) []Stats {
	out := make([]Stats, len(stats))
	for i, s := range stats {
		if s.Models == nil {
			s.Models = map[string]ModelIdentity{}
		}
		out[i] = s
	}
	return out
}

// maxListedRows bounds how many rows MatchRecord names per list.
const maxListedRows = 20

// MatchRecord returns nil when stored, the rows the database holds for an
// attempt, is the attempt the executor recorded (expected): same names,
// entries and stats in order. Otherwise it returns an error wrapping
// ErrCorrupt that names the attempt and lists the unexpected and missing
// rows by identity (task, role, key, digest, producer; stats counts), never
// result bytes. Store.Checkpoint calls it under the job lock.
func MatchRecord(expected, stored Manifest) error {
	if expected.Digest() == stored.Digest() {
		return nil
	}
	describe := func(m Manifest) []string {
		var rows []string
		for _, e := range canonicalEntries(m.Entries) {
			// Task and producer IDs are free text a database writer chooses; %q keeps
			// them from forging lines in the operator log. The other fields are
			// constrained by CHECKs.
			rows = append(rows, fmt.Sprintf("entry(task=%q role=%s key=%s digest=%s producer=%q)",
				e.TaskID, e.Role, e.CacheKey, e.ResultDigest, e.ProducerJobID))
		}
		for i, s := range canonicalStats(m.Stats) {
			models, err := json.Marshal(s.Models)
			if err != nil {
				panic(fmt.Sprintf("resultcache: MatchRecord: json.Marshal of models failed: %v", err))
			}
			rows = append(rows, fmt.Sprintf("stats#%d(mode=%s reason=%q tasks=%d hits=%d dispatched=%d models=%s)",
				i+1, s.Mode, s.Reason, s.Tasks, s.Hits, s.Dispatched, models))
		}
		return rows
	}
	count := func(rows []string) map[string]int {
		n := map[string]int{}
		for _, r := range rows {
			n[r]++
		}
		return n
	}
	exp, got := describe(expected), describe(stored)
	expN, gotN := count(exp), count(got)
	diff := func(rows []string, other map[string]int) []string {
		seen := map[string]int{}
		var out []string
		for _, r := range rows {
			seen[r]++
			if seen[r] > other[r] {
				out = append(out, r)
			}
		}
		return out
	}
	list := func(rows []string) string {
		if len(rows) == 0 {
			return "none"
		}
		if len(rows) > maxListedRows {
			return strings.Join(rows[:maxListedRows], ", ") + fmt.Sprintf(", and %d more", len(rows)-maxListedRows)
		}
		return strings.Join(rows, ", ")
	}
	names := ""
	if expected.TenantID != stored.TenantID || expected.JobID != stored.JobID || expected.Analyzer != stored.Analyzer || expected.Attempt != stored.Attempt {
		names = fmt.Sprintf("; the database rows are of tenant %s job %s analyzer %s attempt %s",
			stored.TenantID, stored.JobID, stored.Analyzer, stored.Attempt)
	}
	return fmt.Errorf("%w: attempt %s of job %s (analyzer %s): the database does not hold what the stage recorded%s; unexpected rows: %s; missing rows: %s",
		ErrCorrupt, expected.Attempt, expected.JobID, expected.Analyzer, names, list(diff(got, expN)), list(diff(exp, gotN)))
}

// written returns the digests m's write entries record under key. A hit
// entry vouches for nothing: the result it reused is its producer's.
func (m Manifest) written(key string) []string {
	var out []string
	for _, e := range m.Entries {
		if e.CacheKey == key && e.Role == RoleWrite {
			out = append(out, e.ResultDigest)
		}
	}
	return out
}

// SignedAttempt is one analyzer's checkpointed attempt and the manifest
// digest its checkpoint signed.
type SignedAttempt struct {
	Attempt string
	Digest  string
}

// Summary is one analyzer's cache use as run status shows it, from one
// checkpointed attempt.
type Summary struct {
	Analyzer string
	Attempt  string
	// Attested reports that the attempt's rows still match its signed
	// digest. When false, the rows changed after signing and the other
	// fields are empty: show the analyzer as unattested.
	Attested bool
	// Stats is the attempt's latest stats row; a zero Mode means it
	// recorded none.
	Stats Stats
	// ProducerJobIDs are the jobs whose results the attempt reused, sorted
	// and distinct, the job itself excluded.
	ProducerJobIDs []string
}

// SummarizeSigned returns one Summary per analyzer in signed, ordered by
// analyzer, from manifests (the rows each analyzer's signed attempt holds
// now): attested with its stats and producers when the manifest's digest
// equals the signed one, unattested and empty otherwise. Store
// implementations call it from VerifiedSummaries.
func SummarizeSigned(manifests map[string]Manifest, signed map[string]SignedAttempt) []Summary {
	var out []Summary
	for _, a := range slices.Sorted(maps.Keys(signed)) {
		sum := Summary{Analyzer: a, Attempt: signed[a].Attempt}
		m, ok := manifests[a]
		if ok && m.Attempt == signed[a].Attempt && m.Digest() == signed[a].Digest {
			sum.Attested = true
			if len(m.Stats) > 0 {
				sum.Stats = m.Stats[len(m.Stats)-1]
			}
			producers := map[string]bool{}
			for _, e := range m.Entries {
				if e.Role == RoleHit && e.ProducerJobID != m.JobID {
					producers[e.ProducerJobID] = true
				}
			}
			sum.ProducerJobIDs = slices.Sorted(maps.Keys(producers))
		}
		out = append(out, sum)
	}
	return out
}

// Store keeps cached results and each job's record of its cache use. Every
// method reads the tenant from ctx and returns an error wrapping
// db.ErrTenantRequired, changing nothing, when there is none; no method
// reads or writes another tenant's rows. Implementations must be safe for
// concurrent use.
//
// Trust flows only through signed cache checkpoints: Checkpoint seals an
// attempt's record, LookupVerifiable plus Snapshot.Verify reuse a result,
// VerifiedSummaries shows an attempt. Producers and Manifest return no
// result bytes and vouch for nothing.
type Store interface {
	// Producers returns, for each key of analyzer that has a cached row,
	// the job that stored it. Use it to learn whose checkpoints to load
	// before LookupVerifiable.
	Producers(ctx context.Context, analyzer string, keys []string) (map[string]string, error)
	// LookupVerifiable reads, in one REPEATABLE READ transaction, the
	// cached rows of analyzer under keys and, for every attempt in
	// attempts whose job stored one of them, that attempt's manifest. The
	// single snapshot is what makes the result fail closed: a row planted
	// in a signed attempt before the snapshot is in its manifest, which then
	// mismatches its signature, and one committed after is invisible, so
	// no lock is taken. Load the checkpoints that attempts names before
	// calling it: one signed after the snapshot could cover rows the
	// snapshot does not hold. An attempt for another analyzer, or an
	// invalid one, wraps ErrInvalidEntry; repeated attempts count once.
	LookupVerifiable(ctx context.Context, analyzer string, keys []string, attempts []AttemptRef) (Snapshot, error)
	// Put stores e's result under its key, replacing any earlier result,
	// response model and producer, and records e's job's write entry, in one
	// transaction: no cache row is ever left without the write entry its
	// verification needs. An invalid e wraps ErrInvalidEntry; a job that is
	// not running wraps ErrJobNotRunning; a job or catalog the tenant does
	// not have is refused.
	Put(ctx context.Context, e Entry) error
	// RecordStage records rec's hit entries (an entry already recorded is
	// kept once) and appends its stats row, in one transaction. An invalid
	// rec wraps ErrInvalidEntry; a job that is not running wraps
	// ErrJobNotRunning; a job the tenant does not have wraps ErrUnknownJob.
	RecordStage(ctx context.Context, rec StageRecord) error
	// Manifest returns the rows attempt of jobID recorded for analyzer,
	// entries in CompareEntries order, stats oldest first, as the database
	// holds them now: an audit read, never a basis for trust. No production
	// path calls it yet (tests and the contract do); an auditor read path
	// over it is a planned follow-up. An invalid attempt wraps
	// ErrInvalidEntry.
	Manifest(ctx context.Context, jobID, analyzer, attempt string) (Manifest, error)
	// Checkpoint seals the attempt expected names. Holding the job row FOR
	// UPDATE, which every cache insert's FOR SHARE waits on, it reads the
	// attempt's rows and compares them with expected, the executor's
	// in-process record (MatchRecord). On a mismatch it returns that error,
	// wrapping ErrCorrupt, without calling seal. Otherwise it calls seal
	// with expected.Digest(), still under the lock. seal only signs that
	// digest: it is CPU work, and must do no object-storage I/O, not touch
	// the job's row and not call the store, any of which would hold the
	// lock that every write to the job's cache record waits on. The caller
	// stores the signed checkpoint after Checkpoint returns and the lock is
	// released. seal's error is returned wrapped. A job the tenant does not
	// have wraps ErrUnknownJob; expected naming another tenant or an
	// invalid attempt wraps ErrInvalidEntry.
	Checkpoint(ctx context.Context, expected Manifest, seal func(ctx context.Context, digest string) error) error
	// VerifiedSummaries reads, for each analyzer in signed, the rows of the
	// signed attempt of jobID and returns SummarizeSigned of them: attested
	// with stats and producers when they still match the signed digest,
	// unattested otherwise. Pass only attempts whose checkpoint signature
	// verified. An invalid attempt wraps ErrInvalidEntry.
	VerifiedSummaries(ctx context.Context, jobID string, signed map[string]SignedAttempt) ([]Summary, error)
}

// MaxAttemptBytes bounds an attempt ID. The engine uses a canonical UUID
// (36 bytes); 64 leaves room for another opaque ID scheme while keeping
// job_cache_entries' nine-column primary key small, and refuses garbage.
const MaxAttemptBytes = 64

// ValidAttempt returns an error wrapping ErrInvalidEntry unless attempt is
// a non-empty attempt ID of at most MaxAttemptBytes of UTF-8 text without
// NUL, which the TEXT column would refuse with a driver error.
func ValidAttempt(attempt string) error {
	return invalid(attemptProblems(attempt))
}

// ValidAttemptRefs returns an error wrapping ErrInvalidEntry naming every
// ref that has no job, is for an analyzer other than analyzer, or has an
// invalid attempt. Store.LookupVerifiable refuses such refs rather than
// skipping them.
func ValidAttemptRefs(analyzer string, refs []AttemptRef) error {
	var p []string
	for i, r := range refs {
		if r.JobID == "" {
			p = append(p, fmt.Sprintf("attempt ref %d: job_id is empty", i))
		}
		if r.Analyzer != analyzer {
			p = append(p, fmt.Sprintf("attempt ref %d (attempt %s of job %s) is for analyzer %q, not %q", i, r.Attempt, r.JobID, r.Analyzer, analyzer))
		}
		for _, q := range attemptProblems(r.Attempt) {
			p = append(p, fmt.Sprintf("attempt ref %d: %s", i, q))
		}
	}
	return invalid(p)
}

// ValidCheckpoint returns an error wrapping ErrInvalidEntry unless expected
// names tenantID, a job, an analyzer and a valid attempt.
func ValidCheckpoint(tenantID string, expected Manifest) error {
	var p []string
	if expected.TenantID != tenantID {
		p = append(p, fmt.Sprintf("the record is of tenant %q, not the caller's tenant", expected.TenantID))
	}
	if expected.JobID == "" {
		p = append(p, "job_id is empty")
	}
	if expected.Analyzer == "" {
		p = append(p, "analyzer is empty")
	}
	p = append(p, attemptProblems(expected.Attempt)...)
	return invalid(p)
}

func attemptProblems(attempt string) []string {
	switch {
	case attempt == "":
		return []string{"attempt is empty"}
	case len(attempt) > MaxAttemptBytes:
		return []string{fmt.Sprintf("attempt is %d bytes, over the %d-byte limit", len(attempt), MaxAttemptBytes)}
	case !utf8.ValidString(attempt) || strings.ContainsRune(attempt, 0):
		return []string{"attempt is not UTF-8 text without NUL"}
	}
	return nil
}

func validKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validDigest(s string) bool {
	rest, ok := strings.CutPrefix(s, "sha256:")
	return ok && validKey(rest)
}

// invalid joins problems into one error wrapping ErrInvalidEntry, or
// returns nil when there are none.
func invalid(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidEntry, strings.Join(problems, "; "))
}
