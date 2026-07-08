package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

func TestRequiresCandidates(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RequiresCandidates Suite")
}

var _ = Describe("RequiresCandidateProvider", func() {
	var (
		mockDB   *mockTenantConnection
		provider *pipeline.RequiresCandidateProvider
		ctx      context.Context
	)

	BeforeEach(func() {
		mockDB = &mockTenantConnection{}
		provider = pipeline.NewRequiresCandidateProvider(mockDB)
		ctx = context.Background()
	})

	Describe("Candidates", func() {
		It("retrieves candidates from the database", func() {
			tenantID := "tenant-1"
			jobID := "job-123"

			provenance1 := []requires.CandidateProvenance{
				{
					GeneratorName: "semantic",
					Score:         0.9,
					Weight:        1.0,
					Metadata:      map[string]string{"model": "text-embedding-3-small"},
				},
			}
			provenance2 := []requires.CandidateProvenance{
				{
					GeneratorName: "keyword",
					Score:         0.7,
					Weight:        0.5,
					Metadata:      map[string]string{"method": "jaccard"},
				},
			}

			provJSON1, _ := json.Marshal(provenance1)
			provJSON2, _ := json.Marshal(provenance2)

			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					// SET LOCAL app.current_tenant
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					Expect(args).To(HaveLen(2))
					Expect(args[0]).To(Equal(tenantID))
					Expect(args[1]).To(Equal(jobID))
					return &mockRows{
						rows: [][]any{
							{"ctrl-001", "ctrl-002", 0.9, provJSON1},
							{"ctrl-001", "ctrl-003", 0.7, provJSON2},
						},
						cursor: 0,
					}, nil
				},
				commitFunc: func() error { return nil },
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			pairs, err := provider.Candidates(ctx, tenantID, jobID)

			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(HaveLen(2))

			Expect(pairs[0].SourceControlID).To(Equal("ctrl-001"))
			Expect(pairs[0].TargetControlID).To(Equal("ctrl-002"))
			Expect(pairs[0].AggregateScore).To(Equal(0.9))
			Expect(pairs[0].Provenance).To(HaveLen(1))
			Expect(pairs[0].Provenance[0].GeneratorName).To(Equal("semantic"))
			Expect(pairs[0].Provenance[0].Score).To(Equal(0.9))

			Expect(pairs[1].SourceControlID).To(Equal("ctrl-001"))
			Expect(pairs[1].TargetControlID).To(Equal("ctrl-003"))
			Expect(pairs[1].AggregateScore).To(Equal(0.7))
			Expect(pairs[1].Provenance).To(HaveLen(1))
			Expect(pairs[1].Provenance[0].GeneratorName).To(Equal("keyword"))
		})

		It("returns empty slice when no candidates exist", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{rows: [][]any{}, cursor: 0}, nil
				},
				commitFunc: func() error { return nil },
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			pairs, err := provider.Candidates(ctx, "tenant-1", "job-456")

			Expect(err).NotTo(HaveOccurred())
			Expect(pairs).To(BeEmpty())
		})

		It("validates tenant ID", func() {
			_, err := provider.Candidates(ctx, "", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant"))
		})

		It("validates job ID", func() {
			_, err := provider.Candidates(ctx, "tenant-1", "")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("job_id is required"))
		})

		It("handles transaction begin errors", func() {
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection failed")
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("beginning transaction"))
		})

		It("handles tenant setting errors", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return errors.New("SET LOCAL failed")
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("setting tenant"))
		})

		It("handles query errors", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return nil, errors.New("query failed")
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("query failed"))
		})

		It("handles scan errors", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{
						rows:   [][]any{{"invalid", "data"}}, // Wrong number of columns
						cursor: 0,
					}, nil
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("scanning row"))
		})

		It("handles invalid JSON in provenance", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{
						rows: [][]any{
							{"ctrl-001", "ctrl-002", 0.9, []byte("invalid json")},
						},
						cursor: 0,
					}, nil
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unmarshaling provenance"))
		})

		It("handles rows.Err() failures", func() {
			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{
						rows:    [][]any{},
						cursor:  0,
						rowsErr: errors.New("iteration error"),
					}, nil
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rows error"))
		})

		It("handles commit failures", func() {
			provenance := []requires.CandidateProvenance{
				{GeneratorName: "test", Score: 0.5, Weight: 1.0, Metadata: map[string]string{}},
			}
			provJSON, _ := json.Marshal(provenance)

			mockTx := &mockTransaction{
				execFunc: func(ctx context.Context, query string, args ...any) error {
					return nil
				},
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{
						rows:   [][]any{{"ctrl-001", "ctrl-002", 0.5, provJSON}},
						cursor: 0,
					}, nil
				},
				commitFunc: func() error {
					return errors.New("commit failed")
				},
			}
			mockDB.beginFunc = func(ctx context.Context) (db.Transaction, error) {
				return mockTx, nil
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("commit failed"))
		})
	})
})

// Mock implementations

type mockTenantConnection struct {
	beginFunc func(ctx context.Context) (db.Transaction, error)
}

func (m *mockTenantConnection) Begin(ctx context.Context) (db.Transaction, error) {
	if m.beginFunc != nil {
		return m.beginFunc(ctx)
	}
	return nil, errors.New("not implemented")
}

func (m *mockTenantConnection) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	return nil, errors.New("Query not allowed on TenantConnection outside transaction")
}

func (m *mockTenantConnection) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	panic("QueryRow not allowed on TenantConnection outside transaction")
}

func (m *mockTenantConnection) Exec(ctx context.Context, query string, args ...any) error {
	return errors.New("Exec not allowed on TenantConnection outside transaction")
}

func (m *mockTenantConnection) Close() error {
	return nil
}

type mockTransaction struct {
	execFunc     func(ctx context.Context, query string, args ...any) error
	queryFunc    func(ctx context.Context, query string, args ...any) (db.Rows, error)
	queryRowFunc func(ctx context.Context, query string, args ...any) db.Row
	commitFunc   func() error
	rollbackFunc func() error
}

func (m *mockTransaction) Commit() error {
	if m.commitFunc != nil {
		return m.commitFunc()
	}
	return nil
}

func (m *mockTransaction) Rollback() error {
	if m.rollbackFunc != nil {
		return m.rollbackFunc()
	}
	return nil
}

func (m *mockTransaction) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if m.queryFunc != nil {
		return m.queryFunc(ctx, query, args...)
	}
	return nil, errors.New("not implemented")
}

func (m *mockTransaction) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if m.queryRowFunc != nil {
		return m.queryRowFunc(ctx, query, args...)
	}
	panic("not implemented")
}

func (m *mockTransaction) Exec(ctx context.Context, query string, args ...any) error {
	if m.execFunc != nil {
		return m.execFunc(ctx, query, args...)
	}
	return errors.New("not implemented")
}

type mockRows struct {
	rows    [][]any
	cursor  int
	rowsErr error
}

func (m *mockRows) Next() bool {
	if m.cursor < len(m.rows) {
		m.cursor++
		return true
	}
	return false
}

func (m *mockRows) Scan(dest ...any) error {
	if m.cursor == 0 || m.cursor > len(m.rows) {
		return errors.New("no current row")
	}
	row := m.rows[m.cursor-1]
	if len(row) != len(dest) {
		return errors.New("column count mismatch")
	}
	for i, val := range row {
		switch d := dest[i].(type) {
		case *string:
			*d = val.(string)
		case *float64:
			*d = val.(float64)
		case *[]byte:
			*d = val.([]byte)
		default:
			return errors.New("unsupported scan type")
		}
	}
	return nil
}

func (m *mockRows) Close() error {
	return nil
}

func (m *mockRows) Err() error {
	return m.rowsErr
}
