package oscal

import "errors"

var (
	ErrInvalidFormat    = errors.New("invalid OSCAL format")
	ErrValidationFailed = errors.New("OSCAL validation failed")
	ErrControlNotFound  = errors.New("control not found")
	ErrParseFailed      = errors.New("failed to parse OSCAL catalog")
	ErrSchemaLoad       = errors.New("failed to load OSCAL schema")
	ErrNoControls       = errors.New("catalog contains no controls")
	ErrStructureFailed  = errors.New("all structuring tiers failed")
	// ErrInvalidPrintArtifactPattern means a StructureOptions print-artefact
	// pattern does not compile or matches empty text.
	ErrInvalidPrintArtifactPattern = errors.New("invalid print-artefact pattern")
)
