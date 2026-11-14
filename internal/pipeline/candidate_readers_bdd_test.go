package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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

	It("leaves out sections and withdrawn controls and keeps a control without text for controlsWithText", func() {
		// Columns: control_id, identifier, title, statement, class, parent_id, container_key, props.
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return dbtest.Rows(nil), nil
				}
				return dbtest.Rows(nil,
					[]any{"cat-1/sec", "sec", "Overview", "Read on.", "compliance-section", "", "", []byte(`{}`)},
					[]any{"cat-1/old", "old", "", "Gone.", "", "", "", []byte(`{"status":"withdrawn"}`)},
					[]any{"cat-1/blank", "blank", "", " ", "", "", "", []byte(`null`)},
					[]any{"cat-1/kept", "kept", "", "Do it.", "", "", "", []byte(nil)},
				), nil
			},
			QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
				return dbtest.Row(sql.ErrNoRows)
			},
		}
		reader := pipeline.NewPGControlsReader(&dbtest.Conn{Tx: mockTx})
		data, err := reader.ControlData(context.Background(), "tenant-1", "cat-1", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(HaveLen(2))
		Expect(data).To(HaveKey("cat-1/kept"))
		Expect(data).To(HaveKey("cat-1/blank"))
	})

	It("resolves each control's context from its raw parent_id, including excluded parents", func() {
		// Columns: control_id, identifier, title, statement, class, parent_id, container_key, props.
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return dbtest.Rows(nil), nil
				}
				return dbtest.Rows(nil,
					[]any{"cat-1/ac-2", "ac-2", "Account Management", "", "compliance-section", "ac", "", []byte(`{}`)},
					[]any{"cat-1/ac", "ac", "Access Control", "", "compliance-section", "", "", []byte(`{}`)},
					[]any{"cat-1/ac-2.a", "ac-2.a", "", "Define account types.", "", "ac-2", "", []byte(`{}`)},
					[]any{"cat-1/ac-2.7", "ac-2.7", "Privileged Accounts", "Manage them.", "", "ac-2", "", []byte(`{}`)},
					[]any{"cat-1/orphan", "orphan", "", "No known parent.", "", "cat-1/ac-2", "", []byte(`{}`)},
				), nil
			},
			QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
				return dbtest.Row(sql.ErrNoRows)
			},
		}
		reader := pipeline.NewPGControlsReader(&dbtest.Conn{Tx: mockTx})
		data, err := reader.ControlData(context.Background(), "tenant-1", "cat-1", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(HaveLen(3), "excluded rows supply titles but are not emitted")
		Expect(data["cat-1/ac-2.a"].Ancestor).To(Equal("Access Control (ac) > Account Management (ac-2)"))
		Expect(data["cat-1/ac-2.7"].Ancestor).To(Equal("Access Control (ac) > Account Management (ac-2)"))
		Expect(data["cat-1/orphan"].Ancestor).To(BeEmpty())
	})

	It("continues a top-level control's context into its containers and never emits a container", func() {
		var containerArgs []any
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					containerArgs = args
					// Columns: container_key, parent_key, title, identifier, label.
					return dbtest.Rows(nil,
						[]any{"g1", "", "Access Control", "ac", "AC"},
						[]any{"g1.1", "g1", "Account Rules", "", ""},
					), nil
				}
				// Columns: control_id, identifier, title, statement, class, parent_id, container_key, props.
				return dbtest.Rows(nil,
					[]any{"cat-1/ac-1", "ac-1", "Policy", "Write a policy.", "", "", "g1", []byte(`{}`)},
					[]any{"cat-1/ac-1.a", "ac-1.a", "", "Review it.", "", "ac-1", "g1", []byte(`{}`)},
					[]any{"cat-1/ac-2", "ac-2", "Accounts", "Manage accounts.", "", "", "g1.1", []byte(`{}`)},
					[]any{"cat-1/ghost", "ghost", "", "Unknown container.", "", "", "g9", []byte(`{}`)},
					[]any{"cat-1/loose", "loose", "", "Ungrouped.", "", "", "", []byte(`{}`)},
				), nil
			},
			QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
				return dbtest.Row(sql.ErrNoRows)
			},
		}
		reader := pipeline.NewPGControlsReader(&dbtest.Conn{Tx: mockTx})
		data, err := reader.ControlData(context.Background(), "tenant-1", "cat-1", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(containerArgs).To(Equal([]any{"tenant-1", "cat-1"}), "containers are read tenant- and catalog-scoped")
		Expect(data).To(HaveLen(5), "only controls are emitted, never containers")
		Expect(data["cat-1/ac-1"].Ancestor).To(Equal("Access Control (AC)"))
		Expect(data["cat-1/ac-1.a"].Ancestor).To(Equal("Access Control (AC) > Policy (ac-1)"))
		Expect(data["cat-1/ac-2"].Ancestor).To(Equal("Access Control (AC) > Account Rules"), "nested containers are walked root first")
		Expect(data["cat-1/ghost"].Ancestor).To(BeEmpty())
		Expect(data["cat-1/loose"].Ancestor).To(BeEmpty())
	})

	It("returns an error when the containers query fails", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
				if strings.Contains(query, "catalog_containers") {
					return nil, errors.New("boom")
				}
				return dbtest.Rows(nil), nil
			},
		}
		reader := pipeline.NewPGControlsReader(&dbtest.Conn{Tx: mockTx})
		_, err := reader.ControlData(context.Background(), "tenant-1", "cat-1", "job-1")
		Expect(err).To(MatchError(ContainSubstring("querying containers")))
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
	It("restricts both sides of the join to the job's catalogs and the tenant", func() {
		var gotQuery string
		var gotArgs []any
		mockTx := &dbtest.Tx{
			QueryFunc: func(_ context.Context, query string, args ...any) (db.Rows, error) {
				gotQuery, gotArgs = query, args
				return dbtest.Rows(nil), nil
			},
		}
		reader := pipeline.NewPGEmbeddingsReader(&dbtest.Conn{Tx: mockTx})
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"cat-src", "cat-tgt"}, "model-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(gotQuery).To(And(
			ContainSubstring("a.tenant_id = $1 AND b.tenant_id = $1"),
			ContainSubstring("a.catalog_id = ANY($2::text[]) AND b.catalog_id = ANY($2::text[])"),
		))
		Expect(gotArgs).To(Equal([]any{"tenant-1", []string{"cat-src", "cat-tgt"}, "model-1"}))
	})

	It("counts only embeddings filed under each control's current catalog, on both sides", func() {
		var gotQuery string
		mockTx := &dbtest.Tx{
			QueryFunc: func(_ context.Context, query string, _ ...any) (db.Rows, error) {
				gotQuery = query
				return dbtest.Rows(nil), nil
			},
		}
		reader := pipeline.NewPGEmbeddingsReader(&dbtest.Conn{Tx: mockTx})
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"cat-src", "cat-tgt"}, "model-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(gotQuery).To(And(
			ContainSubstring("ca.tenant_id = a.tenant_id AND ca.control_id = a.control_id AND ca.catalog_id = a.catalog_id"),
			ContainSubstring("cb.tenant_id = b.tenant_id AND cb.control_id = b.control_id AND cb.catalog_id = b.catalog_id"),
		))
	})

	It("refuses a pair scanned twice instead of keeping either similarity", func() {
		mockTx := &dbtest.Tx{
			QueryFunc: func(context.Context, string, ...any) (db.Rows, error) {
				return dbtest.Rows(nil,
					[]any{"ctrl-001", "ctrl-002", 90.0},
					[]any{"ctrl-001", "ctrl-002", 40.0},
				), nil
			},
		}
		reader := pipeline.NewPGEmbeddingsReader(&dbtest.Conn{Tx: mockTx})
		ids, values, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"catalog-1"}, "model-1")
		Expect(err).To(MatchError(And(ContainSubstring("ctrl-001"), ContainSubstring("ctrl-002"), ContainSubstring("more than once"))))
		Expect(ids).To(BeNil())
		Expect(values).To(BeNil())
	})

	It("refuses an empty catalog list before any transaction starts", func() {
		conn := &dbtest.Conn{}
		reader := pipeline.NewPGEmbeddingsReader(conn)
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", nil, "model-1")
		Expect(err).To(MatchError(ContainSubstring("no catalog IDs")))
		Expect(conn.Begins()).To(BeZero())
	})

	It("validates the tenant ID", func() {
		conn := &dbtest.Conn{}
		reader := pipeline.NewPGEmbeddingsReader(conn)
		_, _, err := reader.SimilarityMatrix(context.Background(), "", []string{"catalog-1"}, "model-1")
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
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"catalog-1"}, "model-1")
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
		_, _, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"catalog-1"}, "model-1")
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
		ids, values, err := reader.SimilarityMatrix(context.Background(), "tenant-1", []string{"catalog-1"}, "model-1")
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
