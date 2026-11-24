package oscal

import (
	"encoding/json"
	"slices"
)

// OSCAL JSON structure keys per the OSCAL specification.
const (
	// CatalogRootKey is the top-level key of an OSCAL catalog document.
	CatalogRootKey = "catalog"
	// MetadataKey is the key of the metadata object within a catalog.
	MetadataKey = "metadata"
	// ProfileRootKey is the top-level key of an OSCAL profile, which selects
	// controls from a catalog rather than being one.
	ProfileRootKey = "profile"
)

// modelRootKeys are the top-level keys of the OSCAL JSON models: the catalog,
// the profile, and the models that neither are a catalog nor resolve into
// one.
var modelRootKeys = []string{
	CatalogRootKey, ProfileRootKey,
	"component-definition", "system-security-plan", "assessment-plan",
	"assessment-results", "plan-of-action-and-milestones", "mapping-collection",
}

// ModelRootKeys returns the top-level keys of every OSCAL JSON model, the
// catalog and profile first. The slice is a copy.
func ModelRootKeys() []string { return slices.Clone(modelRootKeys) }

// IsModelJSON reports whether data is a JSON object with the top-level key of
// any OSCAL model (ModelRootKeys), whatever its value. Every document
// DetectCatalogJSON calls a catalog is also a model. Callers that route
// documents use it to keep all OSCAL JSON, not only catalogs, on the path
// that can explain why a non-catalog model cannot be imported.
func IsModelJSON(data []byte) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	for _, key := range modelRootKeys {
		if _, ok := root[key]; ok {
			return true
		}
	}
	return false
}

// CatalogJSON is what DetectCatalogJSON reads from a document before its
// controls are parsed.
type CatalogJSON struct {
	// IsCatalog reports whether the document is OSCAL catalog JSON: a JSON
	// object with the CatalogRootKey key, whatever its value.
	IsCatalog bool
	// metadata is the catalog's metadata object, nil when absent or not an
	// object.
	metadata map[string]interface{}
}

// DetectCatalogJSON decodes data once and reports whether it is OSCAL
// catalog JSON. A document that is not a JSON object is not a catalog. It is
// the single content sniff that decides whether a document is imported as
// OSCAL, so every caller that routes by format agrees with the importer.
func DetectCatalogJSON(data []byte) CatalogJSON {
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return CatalogJSON{}
	}
	rawCatalog, ok := root[CatalogRootKey]
	if !ok {
		return CatalogJSON{}
	}
	doc := CatalogJSON{IsCatalog: true}
	if catalog, ok := rawCatalog.(map[string]interface{}); ok {
		doc.metadata, _ = catalog[MetadataKey].(map[string]interface{})
	}
	return doc
}

// MetadataString returns the string field key of the catalog's metadata
// (for example "title" or "version"), or "" when the document is not a
// catalog or the field is absent or not a string.
func (d CatalogJSON) MetadataString(key string) string {
	value, _ := d.metadata[key].(string)
	return value
}
