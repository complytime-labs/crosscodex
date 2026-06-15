package oscal

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateSchema validates raw JSON bytes against an OSCAL JSON Schema file.
func ValidateSchema(data []byte, schemaPath string) error {
	schemaData, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("failed to load OSCAL schema at %s: %w", schemaPath, ErrSchemaLoad)
	}

	compiler := jsonschema.NewCompiler()

	// Parse schema document
	var schemaDoc any
	if err := json.Unmarshal(schemaData, &schemaDoc); err != nil {
		return fmt.Errorf("failed to parse OSCAL schema: %w", ErrSchemaLoad)
	}

	// Add schema resource to compiler
	if err := compiler.AddResource(schemaPath, schemaDoc); err != nil {
		return fmt.Errorf("failed to add OSCAL schema resource: %w", ErrSchemaLoad)
	}

	// Compile schema
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return fmt.Errorf("failed to compile OSCAL schema: %w", ErrSchemaLoad)
	}

	// Parse data document
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("invalid JSON: %w", ErrInvalidFormat)
	}

	// Validate document against schema
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("OSCAL schema validation: %s: %w", err, ErrValidationFailed)
	}

	return nil
}
