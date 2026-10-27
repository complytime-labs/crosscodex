package pipeline_test

import (
	"context"
	"database/sql"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var _ = Describe("PGControlsReader", func() {
	It("validates the tenant ID", func() {
		conn := &dbtest.Conn{}
		reader := pipeline.NewPGControlsReader(conn)
		_, err := reader.ControlData(context.Background(), "", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error for empty tenant ID")
		Expect(conn.Begins()).To(BeZero(), "the tenant must be refused before any transaction starts")
	})

	It("returns an error when Begin fails", func() {
		mockDB := &dbtest.Conn{
			BeginFunc: func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection refused")
			},
		}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error when Begin fails")
	})

	It("returns an error when Query fails", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return nil, errors.New("query failed")
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error when Query fails")
	})

	It("returns an error on column-count mismatch during scan", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				// Wrong column count makes dbtest.Rows' Scan fail.
				return dbtest.Rows(nil, []any{"AC-1", "title-only"}), nil
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error on column-count mismatch during scan")
	})

	It("tolerates sql.ErrNoRows from the classify lookup", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return dbtest.Rows(nil), nil
			},
			QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
				// pgControlsReader.ControlData tolerates this via errors.Is(err,
				// sql.ErrNoRows); a plain errors.New would not satisfy that check.
				return dbtest.Row(sql.ErrNoRows)
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGControlsReader(mockDB)
		data, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).NotTo(HaveOccurred(), "expected no-classify-yet to be tolerated (empty Type/Level)")
		Expect(data).NotTo(BeNil(), "expected non-nil (possibly empty) map")
	})

	It("returns an error on malformed classify JSON", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return dbtest.Rows(nil), nil
			},
			QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
				return dbtest.Row(nil, []byte("not json"))
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGControlsReader(mockDB)
		_, err := reader.ControlData(context.Background(), "tenant-1", "catalog-1", "job-1")
		Expect(err).To(HaveOccurred(), "expected error on malformed classify JSON")
	})
})

var _ = Describe("PGEmbeddingsReader", func() {
	It("validates the tenant ID", func() {
		conn := &dbtest.Conn{}
		reader := pipeline.NewPGEmbeddingsReader(conn)
		_, _, err := reader.SimilarityMatrix(context.Background(), "", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error for empty tenant ID")
		Expect(conn.Begins()).To(BeZero(), "the tenant must be refused before any transaction starts")
	})

	It("returns an error when Begin fails", func() {
		mockDB := &dbtest.Conn{
			BeginFunc: func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection refused")
			},
		}
		reader := pipeline.NewPGEmbeddingsReader(mockDB)
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error when Begin fails")
	})

	It("returns an error when Query fails", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return nil, errors.New("query failed")
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
		reader := pipeline.NewPGEmbeddingsReader(mockDB)
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", "catalog-1", "model-1")
		Expect(err).To(HaveOccurred(), "expected error when Query fails")
	})

	It("assembles the similarity matrix with correctly indexed, sorted ids", func() {
		// Deliberately asymmetric so a transposition bug (values[ti][si] instead
		// of values[si][ti]) or an id-index mixup would fail these assertions --
		// a symmetric fixture cannot distinguish correct assembly from transposed.
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				return dbtest.Rows(nil,
					[]any{"ctrl-001", "ctrl-002", 90.0},
					[]any{"ctrl-002", "ctrl-001", 70.0},
				), nil
			},
		}
		mockDB := &dbtest.Conn{Tx: mockTx}
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
