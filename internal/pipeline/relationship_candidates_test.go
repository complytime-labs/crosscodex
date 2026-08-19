package pipeline_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

// Ginkgo suite bootstrap (RegisterFailHandler/RunSpecs) lives in
// requires_candidates_test.go (TestRequiresCandidates); it registers and
// runs every Describe block in this package, including the ones below.
// Ginkgo forbids calling RunSpecs more than once per test binary, so this
// file must not define its own TestXxx entry point.

var _ = Describe("RelationshipCandidateProvider", func() {
	var (
		mockDB   *mockTenantConnection
		provider *pipeline.RelationshipCandidateProvider
		ctx      context.Context
	)

	BeforeEach(func() {
		mockDB = &mockTenantConnection{}
		provider = pipeline.NewRelationshipCandidateProvider(mockDB)
		ctx = context.Background()
	})

	Describe("Candidates", func() {
		It("retrieves candidates from the database", func() {
			tenantID := "tenant-1"
			jobID := "job-123"

			mockTx := &mockTransaction{
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					Expect(args).To(HaveLen(2))
					Expect(args[0]).To(Equal(tenantID))
					Expect(args[1]).To(Equal(jobID))
					return &mockRows{
						rows: [][]any{
							{"ctrl-001", "ctrl-002", 0.9},
							{"ctrl-001", "ctrl-003", 0.7},
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
			Expect(pairs[0].SimilarityScore).To(Equal(float32(0.9)))

			Expect(pairs[1].SourceControlID).To(Equal("ctrl-001"))
			Expect(pairs[1].TargetControlID).To(Equal("ctrl-003"))
			Expect(pairs[1].SimilarityScore).To(Equal(float32(0.7)))
		})

		It("returns empty slice when no candidates exist", func() {
			mockTx := &mockTransaction{
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

		It("handles query errors", func() {
			mockTx := &mockTransaction{
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

		It("handles rows.Err() failures", func() {
			mockTx := &mockTransaction{
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
			mockTx := &mockTransaction{
				queryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return &mockRows{
						rows:   [][]any{{"ctrl-001", "ctrl-002", 0.5}},
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
