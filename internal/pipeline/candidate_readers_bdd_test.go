package pipeline_test

import (
	"context"
	"database/sql"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("PGControlsReader", func() {
	It("validates the tenant ID", func() {
		reader := pipeline.NewPGControlsReader(&mockTenantConnection{})
		_, err := reader.ControlData(context.Background(), "", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error for empty tenant ID")
	})

	It("returns an error when Begin fails", func() {
		mockDB := &mockTenantConnection{
			beginFunc: func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection refused")
			},
		}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error when Begin fails")
	})

	It("returns an error when Query fails", func() {
		mockTx := &mockTransaction{
			queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return nil, errors.New("query failed")
			},
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error when Query fails")
	})

	It("returns an error on column-count mismatch during scan", func() {
		mockTx := &mockTransaction{
			queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				// Wrong column count triggers mockRows.Scan's "column count mismatch".
				return &mockRows{rows: [][]any{{"AC-1", "title-only"}}, cursor: 0}, nil
			},
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error on column-count mismatch during scan")
	})

	It("tolerates sql.ErrNoRows from the classify lookup", func() {
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
		Expect(err).NotTo(HaveOccurred(), "expected no-classify-yet to be tolerated (empty Type/Level)")
		Expect(data).NotTo(BeNil(), "expected non-nil (possibly empty) map")
	})

	It("returns an error on malformed classify JSON", func() {
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
		Expect(err).To(HaveOccurred(), "expected error on malformed classify JSON")
	})
})

var _ = Describe("PGEmbeddingsReader", func() {
	It("validates the tenant ID", func() {
		reader := pipeline.NewPGEmbeddingsReader(&mockTenantConnection{})
		_, _, err := reader.SimilarityMatrix(context.Background(), "", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error for empty tenant ID")
	})

	It("returns an error when Begin fails", func() {
		mockDB := &mockTenantConnection{
			beginFunc: func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection refused")
			},
		}
		reader := pipeline.NewPGEmbeddingsReader(mockDB)
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error when Begin fails")
	})

	It("returns an error when Query fails", func() {
		mockTx := &mockTransaction{
			queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return nil, errors.New("query failed")
			},
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}
		reader := pipeline.NewPGEmbeddingsReader(mockDB)
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error when Query fails")
	})

	It("assembles the similarity matrix with correctly indexed, sorted ids", func() {
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
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(HaveLen(2))
		Expect(values).To(HaveLen(2))
		Expect(values[0]).To(HaveLen(2))

		// ids are sorted (candidate_readers.go's SimilarityMatrix sorts them for
		// deterministic indexing), so with "ctrl-001" < "ctrl-002" alphabetically,
		// index 0 = ctrl-001, index 1 = ctrl-002.
		idx := make(map[string]int, len(ids))
		for i, id := range ids {
			idx[id] = i
		}
		Expect(values[idx["ctrl-001"]][idx["ctrl-002"]]).To(BeNumerically("==", 90.0))
		Expect(values[idx["ctrl-002"]][idx["ctrl-001"]]).To(BeNumerically("==", 70.0))
	})
})

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
