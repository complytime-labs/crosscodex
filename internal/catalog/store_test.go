package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// Test compile-time interface check.
func TestPGStoreImplementsStore(t *testing.T) {
	var _ Store = (*PGStore)(nil)
}

// Test ListOptions.EffectiveLimit().
func TestListOptions_EffectiveLimit(t *testing.T) {
	tests := []struct {
		name     string
		opts     ListOptions
		expected int
	}{
		{
			name:     "zero returns default 50",
			opts:     ListOptions{Limit: 0},
			expected: 50,
		},
		{
			name:     "negative returns default 50",
			opts:     ListOptions{Limit: -10},
			expected: 50,
		},
		{
			name:     "within range returns same",
			opts:     ListOptions{Limit: 100},
			expected: 100,
		},
		{
			name:     "max at 1000 returns 1000",
			opts:     ListOptions{Limit: 1000},
			expected: 1000,
		},
		{
			name:     "above 1000 caps at 1000",
			opts:     ListOptions{Limit: 5000},
			expected: 1000,
		},
		{
			name:     "just above 1000 caps at 1000",
			opts:     ListOptions{Limit: 1001},
			expected: 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.opts.EffectiveLimit()
			if got != tt.expected {
				t.Errorf("EffectiveLimit() = %d, want %d", got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mock types implementing db interfaces for unit testing
// ---------------------------------------------------------------------------

type mockConn struct {
	beginFn func(ctx context.Context) (db.Transaction, error)
}

func (m *mockConn) Begin(ctx context.Context) (db.Transaction, error) { return m.beginFn(ctx) }
func (m *mockConn) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return nil, errors.New("mockConn.Query not implemented")
}
func (m *mockConn) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return &mockRow{scanFn: func(...any) error { return errors.New("mockConn.QueryRow not implemented") }}
}
func (m *mockConn) Exec(_ context.Context, _ string, _ ...any) error {
	return errors.New("mockConn.Exec not implemented")
}
func (m *mockConn) Close() error { return nil }

type mockTx struct {
	committed  bool
	rolledBack bool
	execFn     func(ctx context.Context, query string, args ...any) error
	queryFn    func(ctx context.Context, query string, args ...any) (db.Rows, error)
	queryRowFn func(ctx context.Context, query string, args ...any) db.Row
}

func (m *mockTx) Commit() error   { m.committed = true; return nil }
func (m *mockTx) Rollback() error { m.rolledBack = true; return nil }
func (m *mockTx) Exec(ctx context.Context, query string, args ...any) error {
	if m.execFn != nil {
		return m.execFn(ctx, query, args...)
	}
	return nil
}
func (m *mockTx) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, query, args...)
	}
	return &emptyRows{}, nil
}
func (m *mockTx) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, query, args...)
	}
	return &mockRow{scanFn: func(...any) error { return nil }}
}

type mockRow struct {
	scanFn func(dest ...any) error
}

func (m *mockRow) Scan(dest ...any) error { return m.scanFn(dest...) }

type emptyRows struct{}

func (e *emptyRows) Next() bool          { return false }
func (e *emptyRows) Scan(_ ...any) error { return nil }
func (e *emptyRows) Close() error        { return nil }
func (e *emptyRows) Err() error          { return nil }

// ---------------------------------------------------------------------------
// PGStore unit tests
// ---------------------------------------------------------------------------

func TestPGStore_UpsertCatalog_BeginError(t *testing.T) {
	beginErr := errors.New("connection refused")
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) {
			return nil, beginErr
		},
	}
	store := NewPGStore(conn)

	err := store.UpsertCatalog(context.Background(), CatalogRecord{CatalogID: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, beginErr) {
		t.Errorf("expected wrapped beginErr, got: %v", err)
	}
}

func TestPGStore_UpsertCatalog_CommitsOnSuccess(t *testing.T) {
	tx := &mockTx{}
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
	}
	store := NewPGStore(conn)

	err := store.UpsertCatalog(context.Background(), CatalogRecord{CatalogID: "cat-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tx.committed {
		t.Error("expected transaction to be committed")
	}
}

func TestPGStore_UpsertCatalog_ExecError(t *testing.T) {
	execErr := errors.New("unique violation")
	tx := &mockTx{
		execFn: func(context.Context, string, ...any) error { return execErr },
	}
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
	}
	store := NewPGStore(conn)

	err := store.UpsertCatalog(context.Background(), CatalogRecord{CatalogID: "cat-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, execErr) {
		t.Errorf("expected execErr, got: %v", err)
	}
	if tx.committed {
		t.Error("transaction should not be committed after exec error")
	}
}

func TestPGStore_UpsertControls_EmptySlice(t *testing.T) {
	// Begin should never be called for empty input.
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) {
			t.Fatal("Begin should not be called for empty controls slice")
			return nil, nil
		},
	}
	store := NewPGStore(conn)

	if err := store.UpsertControls(context.Background(), nil); err != nil {
		t.Fatalf("nil slice: unexpected error: %v", err)
	}
	if err := store.UpsertControls(context.Background(), []ControlRecord{}); err != nil {
		t.Fatalf("empty slice: unexpected error: %v", err)
	}
}

func TestPGStore_UpsertControls_BeginError(t *testing.T) {
	beginErr := errors.New("pool exhausted")
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) { return nil, beginErr },
	}
	store := NewPGStore(conn)

	err := store.UpsertControls(context.Background(), []ControlRecord{{ControlID: "c1"}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, beginErr) {
		t.Errorf("expected wrapped beginErr, got: %v", err)
	}
}

func TestPGStore_UpsertControls_CommitsOnSuccess(t *testing.T) {
	tx := &mockTx{}
	conn := &mockConn{
		beginFn: func(context.Context) (db.Transaction, error) { return tx, nil },
	}
	store := NewPGStore(conn)

	ctrl := ControlRecord{
		ControlID: "ctrl-1",
		CatalogID: "cat-1",
		Title:     "Test Control",
	}
	err := store.UpsertControls(context.Background(), []ControlRecord{ctrl})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !tx.committed {
		t.Error("expected transaction to be committed")
	}
}
