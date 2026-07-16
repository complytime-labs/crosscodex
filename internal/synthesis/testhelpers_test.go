package synthesis_test

import (
	"context"
	"sync"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

// ---------------------------------------------------------------------------
// Mock DB types for Service tests
// ---------------------------------------------------------------------------

// mockRow implements db.Row for test assertions.
type mockRow struct {
	scanFunc func(dest ...any) error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.scanFunc != nil {
		return r.scanFunc(dest...)
	}
	return nil
}

// mockTransaction implements db.Transaction for test assertions.
type mockTransaction struct {
	mu            sync.Mutex
	committed     bool
	rollbackCount int
	execFunc      func(ctx context.Context, query string, args ...any) error
	queryFunc     func(ctx context.Context, query string, args ...any) (db.Rows, error)
	queryRowFn    func(ctx context.Context, query string, args ...any) db.Row
	commitErr     error
	rollbackErr   error
}

func (t *mockTransaction) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.committed = true
	return t.commitErr
}

func (t *mockTransaction) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollbackCount++
	return t.rollbackErr
}

func (t *mockTransaction) Exec(ctx context.Context, query string, args ...any) error {
	if t.execFunc != nil {
		return t.execFunc(ctx, query, args...)
	}
	return nil
}

func (t *mockTransaction) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if t.queryFunc != nil {
		return t.queryFunc(ctx, query, args...)
	}
	return nil, nil
}

func (t *mockTransaction) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if t.queryRowFn != nil {
		return t.queryRowFn(ctx, query, args...)
	}
	// Default: return a row that writes a dummy source_id into the scan dest
	return &mockRow{
		scanFunc: func(dest ...any) error {
			if len(dest) > 0 {
				if p, ok := dest[0].(*string); ok {
					*p = "ok"
				}
			}
			return nil
		},
	}
}

// mockConnection implements db.Connection for test assertions.
type mockConnection struct {
	tx       *mockTransaction
	beginErr error
}

func (c *mockConnection) Begin(_ context.Context) (db.Transaction, error) {
	if c.beginErr != nil {
		return nil, c.beginErr
	}
	return c.tx, nil
}

func (c *mockConnection) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, nil
}

func (c *mockConnection) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return &mockRow{}
}

func (c *mockConnection) Exec(_ context.Context, _ string, _ ...any) error {
	return nil
}

func (c *mockConnection) Close() error {
	return nil
}

// mockRows implements db.Rows for test assertions.
// sourceIDs is the list of source_id values returned by Next()/Scan() calls.
// queryErr is returned by Err() after iteration completes, if non-nil.
type mockRows struct {
	sourceIDs []string
	pos       int
	queryErr  error
	closed    bool
}

func (r *mockRows) Next() bool {
	return r.pos < len(r.sourceIDs)
}

func (r *mockRows) Scan(dest ...any) error {
	if r.pos >= len(r.sourceIDs) {
		return nil
	}
	if len(dest) > 0 {
		if p, ok := dest[0].(*string); ok {
			*p = r.sourceIDs[r.pos]
		}
	}
	r.pos++
	return nil
}

func (r *mockRows) Close() error {
	r.closed = true
	return nil
}

func (r *mockRows) Err() error {
	return r.queryErr
}

// newSuccessMockDB creates a mock connection that succeeds for all DB operations.
// Query returns one source_id row per input row in the batch, simulating a
// successful UNNEST UPDATE...RETURNING.
func newSuccessMockDB() *mockConnection {
	tx := &mockTransaction{
		queryFunc: func(_ context.Context, _ string, args ...any) (db.Rows, error) {
			// args[0] is []float64 viabilities — one per input row.
			// Return one source_id "ok" per element to simulate full match.
			var count int
			if len(args) > 0 {
				if viabilities, ok := args[0].([]float64); ok {
					count = len(viabilities)
				}
			}
			ids := make([]string, count)
			for i := range ids {
				ids[i] = "ok"
			}
			return &mockRows{sourceIDs: ids}, nil
		},
	}
	return &mockConnection{tx: tx}
}

// newErrorMockDB creates a mock connection whose Query returns the given error.
func newErrorMockDB(err error) *mockConnection {
	tx := &mockTransaction{
		queryFunc: func(_ context.Context, _ string, _ ...any) (db.Rows, error) {
			return nil, err
		},
	}
	return &mockConnection{tx: tx}
}

// newNoRowsMockDB creates a mock connection whose Query returns 0 rows,
// simulating a case where no vote_summaries rows matched the update criteria.
func newNoRowsMockDB() *mockConnection {
	tx := &mockTransaction{
		queryFunc: func(_ context.Context, _ string, _ ...any) (db.Rows, error) {
			return &mockRows{sourceIDs: []string{}}, nil
		},
	}
	return &mockConnection{tx: tx}
}

// defaultViabilityConfig returns a ViabilityConfig with spec defaults.
func defaultViabilityConfig() config.ViabilityConfig {
	return config.ViabilityConfig{
		TypeMismatchFactor: 0.8,
		SkipLevelFactor:    0.7,
		IntegralToFactor:   1.1,
	}
}

// defaultAssessmentConfig returns an AssessmentConfig with spec defaults.
func defaultAssessmentConfig() config.AssessmentConfig {
	return config.AssessmentConfig{
		IQRGood:        20.0,
		IQRPoor:        10.0,
		NoRelHigh:      0.97,
		NoRelLow:       0.80,
		ContestedWarn:  0.20,
		ActionableWarn: 0.30,
	}
}

// defaultSynthesisConfig returns a SynthesisConfig with spec defaults.
func defaultSynthesisConfig() config.SynthesisConfig {
	return config.SynthesisConfig{
		Viability:             defaultViabilityConfig(),
		Assessment:            defaultAssessmentConfig(),
		ConfidenceThreshold:   0.5,
		MaxMappingsPerControl: 10,
	}
}

// makeInput creates a SynthesisInput with required fields.
func makeInput(sourceID, targetID string, score float64) synthesis.SynthesisInput {
	return synthesis.SynthesisInput{
		SourceID:              sourceID,
		TargetID:              targetID,
		SimilarityScore:       score,
		ConsensusRelationship: "EQUIVALENT",
		ContributionType:      "",
		ConfidenceFraction:    0.8,
		Unanimous:             true,
	}
}

// makeClassification creates a Classification with type and level.
func makeClassification(typ, level string) synthesis.Classification {
	return synthesis.Classification{Type: typ, Level: level}
}

// findDiagnostic returns the diagnostic with the given category from a report.
func findDiagnostic(report *synthesis.QualityReport, category string) *synthesis.Diagnostic {
	for i := range report.Diagnostics {
		if report.Diagnostics[i].Category == category {
			return &report.Diagnostics[i]
		}
	}
	return nil
}
