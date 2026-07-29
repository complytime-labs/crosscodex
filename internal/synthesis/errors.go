package synthesis

import (
	"errors"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// ErrInvalidInput indicates a SynthesisInput failed validation.
var ErrInvalidInput = errors.New("synthesis: invalid input")

// ErrInvalidJobID indicates an empty job ID was provided.
var ErrInvalidJobID = errors.New("synthesis: invalid job ID")

// ErrDBUpdate indicates the database update failed.
var ErrDBUpdate = errors.New("synthesis: database update failed")

// ErrDBNoRowsAffected indicates no vote_summaries rows were updated.
var ErrDBNoRowsAffected = errors.New("synthesis: no rows affected by viability update")

// ErrImmutabilityViolation indicates the database immutability trigger
// rejected the update (parent job is completed).
var ErrImmutabilityViolation = fmt.Errorf("synthesis: immutability violation — parent job is completed; to update viability, create a new job instead of modifying a completed one: %w", db.ErrImmutableRecord)
