// Package oscal provides OSCAL catalog parsing and validation.
//
// Handles parsing NIST OSCAL JSON/XML formats for compliance frameworks.
//
// Example usage:
//
//	parser := oscal.NewParser()
//	catalog, err := parser.Parse(ctx, file)
//	if err != nil {
//	    return err
//	}
//
//	err = parser.Validate(ctx, catalog)
//	if err != nil {
//	    return fmt.Errorf("invalid OSCAL catalog: %w", err)
//	}
package oscal
