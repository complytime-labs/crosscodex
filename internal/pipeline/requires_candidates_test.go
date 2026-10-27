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
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

func TestRequiresCandidates(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "RequiresCandidates Suite")
}

var _ = Describe("RequiresCandidateProvider", func() {
	var (
		mockDB   *dbtest.Conn
		provider *pipeline.RequiresCandidateProvider
		ctx      context.Context
	)

	BeforeEach(func() {
		mockDB = &dbtest.Conn{}
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

			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					Expect(args).To(HaveLen(2))
					Expect(args[0]).To(Equal(tenantID))
					Expect(args[1]).To(Equal(jobID))
					return dbtest.Rows(nil,
						[]any{"ctrl-001", "ctrl-002", 0.9, provJSON1},
						[]any{"ctrl-001", "ctrl-003", 0.7, provJSON2},
					), nil
				},
			}
			mockDB.Tx = mockTx

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
			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return dbtest.Rows(nil), nil
				},
			}
			mockDB.Tx = mockTx

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
			mockDB.BeginFunc = func(ctx context.Context) (db.Transaction, error) {
				return nil, errors.New("connection failed")
			}

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("beginning transaction"))
		})

		It("handles tenant setting errors", func() {
			mockTx := &dbtest.Tx{
				ExecFunc: func(ctx context.Context, query string, args ...any) error {
					return errors.New("SET LOCAL failed")
				},
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("setting tenant"))
		})

		It("handles query errors", func() {
			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return nil, errors.New("query failed")
				},
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("query failed"))
		})

		It("handles scan errors", func() {
			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return dbtest.Rows(nil, []any{"invalid", "data"}), nil // Wrong number of columns
				},
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("scanning row"))
		})

		It("handles invalid JSON in provenance", func() {
			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return dbtest.Rows(nil,
						[]any{"ctrl-001", "ctrl-002", 0.9, []byte("invalid json")},
					), nil
				},
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unmarshaling provenance"))
		})

		It("handles rows.Err() failures", func() {
			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return dbtest.Rows(errors.New("iteration error")), nil
				},
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rows error"))
		})

		It("handles commit failures", func() {
			provenance := []requires.CandidateProvenance{
				{GeneratorName: "test", Score: 0.5, Weight: 1.0, Metadata: map[string]string{}},
			}
			provJSON, _ := json.Marshal(provenance)

			mockTx := &dbtest.Tx{
				ExecFunc: acceptExec,
				QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
					return dbtest.Rows(nil, []any{"ctrl-001", "ctrl-002", 0.5, provJSON}), nil
				},
				CommitErr: errors.New("commit failed"),
			}
			mockDB.Tx = mockTx

			_, err := provider.Candidates(ctx, "tenant-1", "job-123")

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("commit failed"))
		})
	})
})
