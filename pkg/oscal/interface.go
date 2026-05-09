package oscal

import (
	"context"
	"io"
)

// Parser handles OSCAL catalog parsing.
//
// Implementations must support both JSON and XML formats.
type Parser interface {
	// Parse parses an OSCAL catalog from the provided reader.
	// Detects format automatically based on content.
	Parse(ctx context.Context, data io.Reader) (*Catalog, error)

	// Validate checks that the catalog conforms to OSCAL schema.
	Validate(ctx context.Context, catalog *Catalog) error

	// FindControl locates a control by ID (searches recursively).
	FindControl(catalog *Catalog, controlID string) (*Control, error)
}
