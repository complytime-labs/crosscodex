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

// Page is one page of mappings. Next is nil on the last page.
type Page struct {
	Mappings []Mapping
	Next     *Cursor
}

// Reader lists mappings. Implementations confine reads to the tenant in ctx.
type Reader interface {
	ListMappings(ctx context.Context, f Filter) (Page, error)
}

// ErrJobNotFound means the job does not exist for the tenant in ctx.
var ErrJobNotFound = errors.New("job not found")

// ErrInvalidPageToken means a page token was not produced by EncodeCursor.
var ErrInvalidPageToken = errors.New("invalid page token")

// JobNotCompletedError means the job exists but has not completed, so its
// mappings are not final.
type JobNotCompletedError struct {
	JobID  string
	Status string
}

func (e *JobNotCompletedError) Error() string {
	return fmt.Sprintf("job %s is %s", e.JobID, e.Status)
}
