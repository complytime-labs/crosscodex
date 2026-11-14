package results

import (
	"context"
	"errors"
	"fmt"
)

// Stage selects which pipeline stage's mappings a Filter reads.
type Stage int

const (
	// StageSynthesized reads mappings after synthesis ranked, thresholded and
	// capped them; each carries a viability.
	StageSynthesized Stage = iota + 1
	// StageAnalyzer reads the relationship analyzer's consensus results before
	// synthesis; they carry no viability.
	StageAnalyzer
)

// Cursor is the keyset position a page starts after. Pages are ordered by
// (JobID, SourceID, TargetID).
type Cursor struct {
	JobID    string
	SourceID string
	TargetID string
}

// Filter selects mappings. TenantID must equal the tenant in ctx.
type Filter struct {
	TenantID string
	// JobID scopes to one job, which must have completed. Empty is allowed
	// only with StageSynthesized and a ControlID, and reads across the
	// tenant's completed jobs only; rows of pending, running, failed and
	// cancelled jobs are excluded.
	JobID             string
	Stage             Stage
	ControlID         string   // matches source or target; empty = any
	MinConfidence     float64  // inclusive
	IncludeNonViable  bool     // StageSynthesized only
	RelationshipTypes []string // analyzer names, e.g. "EQUIVALENT"; never empty
	PageSize          int      // > 0
	After             *Cursor  // nil = first page
}

// Mapping is one source→target relationship a job produced.
type Mapping struct {
	JobID            string
	SourceID         string
	TargetID         string
	RelationshipType string // analyzer name, e.g. "EQUIVALENT"
	Confidence       float64
	// Viability is synthesis's viability weight; HasViability is false for
	// StageAnalyzer mappings.
	Viability    float64
	HasViability bool
}

// JobAnalysis is the catalog selection of the job a page lists: what
// produced its mappings.
type JobAnalysis struct {
	Source        CatalogSummary
	Target        CatalogSummary // equals Source within one catalog
	Bidirectional bool
}

// CatalogSummary identifies a catalog for display. Snapshot is true when
// Name and Version are the ones the job recorded at creation; otherwise (a
// job created before snapshots) they are the catalog's current ones. Imported
// is false when the tenant no longer has the catalog; Name and Version are
// then empty unless Snapshot is true.
type CatalogSummary struct {
	ID       string
	Name     string
	Version  string
	Imported bool
	Snapshot bool
}

// Page is one page of mappings. Next is nil on the last page.
type Page struct {
	Mappings []Mapping
	Next     *Cursor
	// Analysis is set when Filter.JobID names a job whose stored config names
	// a catalog; it is read in the same tenant-scoped transaction as the
	// mappings.
	Analysis *JobAnalysis
}

// CandidateFilter selects a completed job's relationship candidates. TenantID
// must equal the tenant in ctx. Pages are ordered by (JobID, SourceID,
// TargetID).
type CandidateFilter struct {
	TenantID  string
	JobID     string  // must name a completed job
	ControlID string  // matches source or target; empty = any
	PageSize  int     // > 0
	After     *Cursor // nil = first page
}

// Contribution is one candidate generator's share of a pair's score, decoded
// from the pair's provenance.
type Contribution struct {
	Generator string
	Score     float64
	Weight    float64
	Metadata  map[string]string
}

// Candidate is one source-target pair the candidate stage produced for a job,
// before any relationship analysis.
type Candidate struct {
	JobID    string
	SourceID string
	TargetID string
	// Similarity is the stored embedding similarity, on the scale of
	// relationship_candidates.similarity_score ([0, 100]).
	Similarity    float64
	Contributions []Contribution
}

// CandidatePage is one page of candidates. Next is nil on the last page.
type CandidatePage struct {
	Candidates []Candidate
	Next       *Cursor
	// Analysis is the catalog selection of the job, as on Page.
	Analysis *JobAnalysis
}

// Reader lists mappings and candidates. Implementations confine reads to the
// tenant in ctx.
type Reader interface {
	ListMappings(ctx context.Context, f Filter) (Page, error)
	ListCandidates(ctx context.Context, f CandidateFilter) (CandidatePage, error)
}

// ErrJobNotFound means the job does not exist for the tenant in ctx.
var ErrJobNotFound = errors.New("job not found")

// ErrInvalidPageToken means a page token was not produced by EncodeCursor.
var ErrInvalidPageToken = errors.New("invalid page token")

// ErrCorruptProvenance means a candidate's stored provenance is not the
// array of generator contributions the pipeline writes.
var ErrCorruptProvenance = errors.New("corrupt candidate provenance")

// JobNotCompletedError means the job exists but has not completed, so its
// mappings are not final.
type JobNotCompletedError struct {
	JobID  string
	Status string
}

func (e *JobNotCompletedError) Error() string {
	return fmt.Sprintf("job %s is %s", e.JobID, e.Status)
}
