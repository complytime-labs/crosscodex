package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

func TestPGControlsReader_ValidatesTenantID(t *testing.T) {
	reader := pipeline.NewPGControlsReader(&mockTenantConnection{})
	_, err := reader.ControlData(context.Background(), "", "catalog-1", "job-1")
	if err == nil {
		t.Fatal("expected error for empty tenant ID")
	}
}

func TestPGControlsReader_BeginFails(t *testing.T) {
	mockDB := &mockTenantConnection{
		beginFunc: func(ctx context.Context) (db.Transaction, error) {
			return nil, errors.New("connection refused")
		},
	}
	reader := pipeline.NewPGControlsReader(mockDB)
	_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
	if err == nil {
		t.Fatal("expected error when Begin fails")
	}
}

func TestPGControlsReader_QueryFails(t *testing.T) {
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return nil, errors.New("query failed")
		},
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGControlsReader(mockDB)
	_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
	if err == nil {
		t.Fatal("expected error when Query fails")
	}
}

func TestPGControlsReader_ScanFails(t *testing.T) {
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			// Wrong column count triggers mockRows.Scan's "column count mismatch".
			return &mockRows{rows: [][]any{{"AC-1", "title-only"}}, cursor: 0}, nil
		},
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGControlsReader(mockDB)
	_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
	if err == nil {
		t.Fatal("expected error on column-count mismatch during scan")
	}
}

func TestPGControlsReader_ClassifyRowsErrTolerated(t *testing.T) {
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return &mockRows{rows: [][]any{}, cursor: 0}, nil
		},
		queryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
			// pgControlsReader.ControlData tolerates this via errors.Is(err,
			// sql.ErrNoRows); a plain errors.New would not satisfy that check.
			return errRow{err: sql.ErrNoRows}
		},
		commitFunc: func() error { return nil },
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGControlsReader(mockDB)
	data, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
	if err != nil {
		t.Fatalf("expected no-classify-yet to be tolerated (empty Type/Level), got error: %v", err)
	}
	if data == nil {
		t.Fatal("expected non-nil (possibly empty) map")
	}
}

func TestPGControlsReader_MalformedClassifyJSON(t *testing.T) {
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return &mockRows{rows: [][]any{}, cursor: 0}, nil
		},
		queryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
			return okRow{data: []byte("not json")}
		},
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGControlsReader(mockDB)
	_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
	if err == nil {
		t.Fatal("expected error on malformed classify JSON")
	}
}

func TestPGEmbeddingsReader_ValidatesTenantID(t *testing.T) {
	reader := pipeline.NewPGEmbeddingsReader(&mockTenantConnection{})
	_, _, err := reader.SimilarityMatrix(context.Background(), "", "catalog-1", "model-1")
	if err == nil {
		t.Fatal("expected error for empty tenant ID")
	}
}

func TestPGEmbeddingsReader_BeginFails(t *testing.T) {
	mockDB := &mockTenantConnection{
		beginFunc: func(ctx context.Context) (db.Transaction, error) {
			return nil, errors.New("connection refused")
		},
	}
	reader := pipeline.NewPGEmbeddingsReader(mockDB)
	_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
	if err == nil {
		t.Fatal("expected error when Begin fails")
	}
}

func TestPGEmbeddingsReader_QueryFails(t *testing.T) {
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return nil, errors.New("query failed")
		},
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGEmbeddingsReader(mockDB)
	_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
	if err == nil {
		t.Fatal("expected error when Query fails")
	}
}

func TestPGEmbeddingsReader_Success(t *testing.T) {
	// Deliberately asymmetric so a transposition bug (values[ti][si] instead
	// of values[si][ti]) or an id-index mixup would fail these assertions --
	// a symmetric fixture cannot distinguish correct assembly from transposed.
	mockTx := &mockTransaction{
		queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			return &mockRows{
				rows: [][]any{
					{"ctrl-001", "ctrl-002", 90.0},
					{"ctrl-002", "ctrl-001", 70.0},
				},
				cursor: 0,
			}, nil
		},
		commitFunc: func() error { return nil },
	}
	mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
	reader := pipeline.NewPGEmbeddingsReader(mockDB)
	ids, values, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
	if err != nil {
		t.Fatalf("SimilarityMatrix: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d: %v", len(ids), ids)
	}
	if len(values) != 2 || len(values[0]) != 2 {
		t.Fatalf("expected 2x2 matrix, got %v", values)
	}

	// ids are sorted (candidate_readers.go's SimilarityMatrix sorts them for
	// deterministic indexing), so with "ctrl-001" < "ctrl-002" alphabetically,
	// index 0 = ctrl-001, index 1 = ctrl-002.
	idx := make(map[string]int, len(ids))
	for i, id := range ids {
		idx[id] = i
	}
	if got := values[idx["ctrl-001"]][idx["ctrl-002"]]; got != 90.0 {
		t.Errorf("values[ctrl-001][ctrl-002] = %v, want 90.0", got)
	}
	if got := values[idx["ctrl-002"]][idx["ctrl-001"]]; got != 70.0 {
		t.Errorf("values[ctrl-002][ctrl-001] = %v, want 70.0", got)
	}
}

// errRow and okRow are minimal db.Row test doubles for QueryRow-based reads.
// db.Row (pkg/db/types.go) declares exactly one method, Scan(dest ...any)
// error, so these satisfy it directly.

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type okRow struct{ data []byte }

func (r okRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("expected 1 scan target")
	}
	if p, ok := dest[0].(*[]byte); ok {
		*p = r.data
		return nil
	}
	return errors.New("unsupported scan target")
}
