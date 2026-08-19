package pipeline_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("WriteRequiresCandidates", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("upserts each pair with its provenance", func() {
		var execCalls []string
		var execArgs [][]any
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				execCalls = append(execCalls, query)
				execArgs = append(execArgs, args)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		pairs := []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.8,
			Provenance: []requires.CandidateProvenance{{GeneratorName: "keyword", Score: 0.8, Weight: 1.0}},
		}}

		err := pipeline.WriteRequiresCandidates(ctx, mockDB, "tenant-1", "job-1", pairs)
		Expect(err).NotTo(HaveOccurred())

		// db.TenantConnection.Begin already establishes the tenant session
		// (see pkg/db/tenant_pool.go); no manual SET LOCAL is expected here.
		Expect(execCalls).To(HaveLen(1))
		Expect(execCalls[0]).To(ContainSubstring("INSERT INTO requires_candidates"))
		Expect(execArgs[0]).To(Equal([]any{"tenant-1", "job-1", "AC-1", "AC-2", 0.8, execArgs[0][5]}))
	})

	It("upsert-writes idempotently: second write for the same pair replaces the first", func() {
		var execCalls []string
		var execArgs [][]any
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				execCalls = append(execCalls, query)
				execArgs = append(execArgs, args)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		firstPairs := []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.4,
			Provenance: []requires.CandidateProvenance{{GeneratorName: "keyword", Score: 0.4, Weight: 1.0}},
		}}
		secondPairs := []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.9,
			Provenance: []requires.CandidateProvenance{{GeneratorName: "semantic", Score: 0.9, Weight: 1.0}},
		}}

		Expect(pipeline.WriteRequiresCandidates(ctx, mockDB, "tenant-1", "job-1", firstPairs)).To(Succeed())
		Expect(pipeline.WriteRequiresCandidates(ctx, mockDB, "tenant-1", "job-1", secondPairs)).To(Succeed())

		Expect(execCalls).To(HaveLen(2))
		for _, q := range execCalls {
			Expect(q).To(ContainSubstring("ON CONFLICT"))
			Expect(strings.ToUpper(q)).To(ContainSubstring("DO UPDATE"))
		}
		// Same upsert query text is reused for both writes; only the bound
		// aggregate_score argument (index 4) differs between the two calls.
		Expect(execCalls[0]).To(Equal(execCalls[1]))
		Expect(execArgs[0][4]).To(Equal(0.4))
		Expect(execArgs[1][4]).To(Equal(0.9))
	})

	It("propagates a begin-transaction error", func() {
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) {
			return nil, errors.New("connection failed")
		}}

		err := pipeline.WriteRequiresCandidates(ctx, mockDB, "tenant-1", "job-1", []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.5,
		}})

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("beginning transaction"))
	})

	It("propagates an exec error and does not commit", func() {
		committed := false
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				return errors.New("exec failed")
			},
			commitFunc: func() error {
				committed = true
				return nil
			},
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		err := pipeline.WriteRequiresCandidates(ctx, mockDB, "tenant-1", "job-1", []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.5,
		}})

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exec failed"))
		Expect(committed).To(BeFalse())
	})

	It("validates the tenant ID", func() {
		mockDB := &mockTenantConnection{}

		err := pipeline.WriteRequiresCandidates(ctx, mockDB, "", "job-1", []requires.RequiresPair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", AggregateScore: 0.5,
		}})

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tenant"))
	})
})

var _ = Describe("WriteRelationshipCandidates", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("upserts each pair with its provenance", func() {
		var execCalls []string
		var execArgs [][]any
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				execCalls = append(execCalls, query)
				execArgs = append(execArgs, args)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		pairs := []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.8,
		}}
		provenance := map[string][]byte{
			"AC-1--AC-2": []byte(`[{"generator":"embedding","score":0.8}]`),
		}

		err := pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", pairs, provenance)
		Expect(err).NotTo(HaveOccurred())

		// db.TenantConnection.Begin already establishes the tenant session
		// (see pkg/db/tenant_pool.go); no manual SET LOCAL is expected here.
		Expect(execCalls).To(HaveLen(1))
		Expect(execCalls[0]).To(ContainSubstring("INSERT INTO relationship_candidates"))
		Expect(execArgs[0]).To(Equal([]any{
			"tenant-1", "job-1", "AC-1", "AC-2", float32(0.8), provenance["AC-1--AC-2"],
		}))
	})

	It("defaults to an empty JSON array when no provenance is supplied for a pair", func() {
		var execArgs [][]any
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				execArgs = append(execArgs, args)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		pairs := []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.8,
		}}

		err := pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", pairs, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(execArgs).To(HaveLen(1))
		Expect(execArgs[0][5]).To(Equal([]byte(`[]`)))
	})

	It("upsert-writes idempotently: second write for the same pair replaces the first", func() {
		var execCalls []string
		var execArgs [][]any
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				execCalls = append(execCalls, query)
				execArgs = append(execArgs, args)
				return nil
			},
			commitFunc: func() error { return nil },
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		firstPairs := []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.4,
		}}
		secondPairs := []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.9,
		}}

		Expect(pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", firstPairs, nil)).To(Succeed())
		Expect(pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", secondPairs, nil)).To(Succeed())

		Expect(execCalls).To(HaveLen(2))
		for _, q := range execCalls {
			Expect(q).To(ContainSubstring("ON CONFLICT"))
			Expect(strings.ToUpper(q)).To(ContainSubstring("DO UPDATE"))
		}
		// Same upsert query text is reused for both writes; only the bound
		// similarity_score argument (index 4) differs between the two calls.
		Expect(execCalls[0]).To(Equal(execCalls[1]))
		Expect(execArgs[0][4]).To(Equal(float32(0.4)))
		Expect(execArgs[1][4]).To(Equal(float32(0.9)))
	})

	It("propagates a begin-transaction error", func() {
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) {
			return nil, errors.New("connection failed")
		}}

		err := pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.5,
		}}, nil)

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("beginning transaction"))
	})

	It("propagates an exec error and does not commit", func() {
		committed := false
		mockTx := &mockTransaction{
			execFunc: func(ctx context.Context, query string, args ...any) error {
				return errors.New("exec failed")
			},
			commitFunc: func() error {
				committed = true
				return nil
			},
		}
		mockDB := &mockTenantConnection{beginFunc: func(ctx context.Context) (db.Transaction, error) { return mockTx, nil }}

		err := pipeline.WriteRelationshipCandidates(ctx, mockDB, "tenant-1", "job-1", []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.5,
		}}, nil)

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exec failed"))
		Expect(committed).To(BeFalse())
	})

	It("validates the tenant ID", func() {
		mockDB := &mockTenantConnection{}

		err := pipeline.WriteRelationshipCandidates(ctx, mockDB, "", "job-1", []relationship.CandidatePair{{
			SourceControlID: "AC-1", TargetControlID: "AC-2", SimilarityScore: 0.5,
		}}, nil)

		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tenant"))
	})
})
