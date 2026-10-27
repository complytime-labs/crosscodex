//go:build !integration

package synthesis_test

import (
	"context"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// sourceIDRows returns one RETURNING source_id row holding id per element of
// args[0], the []float64 viabilities the batch UPDATE binds first, simulating
// an UNNEST UPDATE...RETURNING that matched every input row.
func sourceIDRows(id string, args []any) *dbtest.FakeRows {
	var count int
	if len(args) > 0 {
		if viabilities, ok := args[0].([]float64); ok {
			count = len(viabilities)
		}
	}
	rows := make([][]any, count)
	for i := range rows {
		rows[i] = []any{id}
	}
	return dbtest.Rows(nil, rows...)
}

// newSuccessMockDB creates a fake connection that succeeds for all DB operations.
func newSuccessMockDB() *dbtest.Conn {
	return &dbtest.Conn{Tx: &dbtest.Tx{
		QueryFunc: func(_ context.Context, _ string, args ...any) (db.Rows, error) {
			return sourceIDRows("ok", args), nil
		},
	}}
}

// newErrorMockDB creates a fake connection whose Query returns the given error.
func newErrorMockDB(err error) *dbtest.Conn {
	return &dbtest.Conn{Tx: &dbtest.Tx{
		QueryFunc: func(_ context.Context, _ string, _ ...any) (db.Rows, error) {
			return nil, err
		},
	}}
}

// newNoRowsMockDB creates a fake connection whose Query returns 0 rows,
// simulating a case where no vote_summaries rows matched the update criteria.
func newNoRowsMockDB() *dbtest.Conn {
	return &dbtest.Conn{Tx: &dbtest.Tx{
		QueryFunc: func(_ context.Context, _ string, _ ...any) (db.Rows, error) {
			return dbtest.Rows(nil), nil
		},
	}}
}

// defaultViabilityConfig returns a ViabilityConfig with spec defaults.
func defaultViabilityConfig() config.ViabilityConfig {
	return config.ViabilityConfig{
		TypeMismatchFactor: 0.8,
		SkipLevelFactor:    0.7,
		IntegralToFactor:   1.1,
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
