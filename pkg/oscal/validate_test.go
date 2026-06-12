package oscal

import (
	"os"
	"path/filepath"
	"testing"
)

// Test 1: Passes validation for valid OSCAL catalog against official schema
func TestValidateSchema_ValidCatalog(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "schemas", "oscal", "1.2.2", "json-schema", "oscal_catalog_schema.json")

	// Skip if schema not fetched
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		t.Skip("OSCAL schema not fetched; run 'task dev:fetch-schemas' to download")
	}

	data, err := os.ReadFile(filepath.Join("testdata", "minimal_catalog.json"))
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	err = ValidateSchema(data, schemaPath)
	if err != nil {
		t.Errorf("expected valid catalog to pass validation, got: %v", err)
	}
}

// Test 2: Returns ErrSchemaLoad when schema file is missing
func TestValidateSchema_MissingSchema(t *testing.T) {
	schemaPath := filepath.Join("testdata", "nonexistent_schema.json")

	data := []byte(`{"catalog": {"uuid": "test"}}`)

	err := ValidateSchema(data, schemaPath)
	if err == nil {
		t.Fatal("expected error when schema file is missing, got nil")
	}

	// Check that error wraps ErrSchemaLoad
	if !containsError(err, ErrSchemaLoad) {
		t.Errorf("expected error to wrap ErrSchemaLoad, got: %v", err)
	}
}

// Test 3: Returns ErrInvalidFormat for malformed JSON input
func TestValidateSchema_MalformedJSON(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "schemas", "oscal", "1.2.2", "json-schema", "oscal_catalog_schema.json")

	// Skip if schema not fetched
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		t.Skip("OSCAL schema not fetched; run 'task dev:fetch-schemas' to download")
	}

	data := []byte(`{this is not valid json}`)

	err := ValidateSchema(data, schemaPath)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}

	// Check that error wraps ErrInvalidFormat
	if !containsError(err, ErrInvalidFormat) {
		t.Errorf("expected error to wrap ErrInvalidFormat, got: %v", err)
	}
}

// Test 4: Returns ErrValidationFailed for valid JSON that violates schema
func TestValidateSchema_InvalidOSCAL(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "schemas", "oscal", "1.2.2", "json-schema", "oscal_catalog_schema.json")

	// Skip if schema not fetched
	if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
		t.Skip("OSCAL schema not fetched; run 'task dev:fetch-schemas' to download")
	}

	// Valid JSON but missing required OSCAL catalog fields
	data := []byte(`{"catalog": {}}`)

	err := ValidateSchema(data, schemaPath)
	if err == nil {
		t.Fatal("expected validation error for invalid OSCAL, got nil")
	}

	// Check that error wraps ErrValidationFailed
	if !containsError(err, ErrValidationFailed) {
		t.Errorf("expected error to wrap ErrValidationFailed, got: %v", err)
	}
}

// Test 5: Returns ErrSchemaLoad for malformed schema file
func TestValidateSchema_MalformedSchema(t *testing.T) {
	// Create temporary malformed schema file
	tmpDir := t.TempDir()
	schemaPath := filepath.Join(tmpDir, "bad_schema.json")
	if err := os.WriteFile(schemaPath, []byte(`{not valid json`), 0600); err != nil {
		t.Fatalf("failed to create temp schema: %v", err)
	}

	data := []byte(`{"catalog": {"uuid": "test"}}`)

	err := ValidateSchema(data, schemaPath)
	if err == nil {
		t.Fatal("expected error for malformed schema, got nil")
	}

	// Check that error wraps ErrSchemaLoad
	if !containsError(err, ErrSchemaLoad) {
		t.Errorf("expected error to wrap ErrSchemaLoad, got: %v", err)
	}
}

// Helper: checks if error chain contains target error
func containsError(err, target error) bool {
	if err == nil {
		return target == nil
	}
	for {
		if err == target {
			return true
		}
		unwrapped := unwrapError(err)
		if unwrapped == nil {
			return false
		}
		err = unwrapped
	}
}

// Helper: unwraps error using interface method
func unwrapError(err error) error {
	type unwrapper interface {
		Unwrap() error
	}
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}
