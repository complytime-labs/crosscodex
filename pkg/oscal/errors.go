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
	// ErrInvalidItems means BuildTree was given items whose IDs or links
	// cannot form a tree; the wrapped message names the item.
	ErrInvalidItems = errors.New("invalid structured items")
	// ErrInvalidPrintArtifactPattern means a StructureOptions print-artefact
	// pattern does not compile or matches empty text.
	ErrInvalidPrintArtifactPattern = errors.New("invalid print-artefact pattern")
	// ErrMappingExport means a job export cannot be turned into a valid OSCAL
	// mapping-collection; the wrapped message says what to change.
	ErrMappingExport = errors.New("oscal mapping export")
)
