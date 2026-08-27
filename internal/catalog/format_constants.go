package catalog

// Catalog format type strings used throughout the service.
// These match the stored provenance format field and are mapped
// to protobuf enum values via formatStringToEnum.
const (
	// FormatOSCAL represents OSCAL JSON catalog format
	FormatOSCAL = "oscal"

	// FormatGemara represents Gemara (unstructured text) catalog format
	FormatGemara = "gemara"

	// SourceTypeDocument represents a catalog sourced from a document ID
	SourceTypeDocument = "document"
)

// OSCAL JSON structure keys per OSCAL specification
const (
	// OSCALRootKey is the top-level key in OSCAL JSON catalogs
	OSCALRootKey = "catalog"

	// OSCALMetadataKey is the metadata object key within a catalog
	OSCALMetadataKey = "metadata"

	// OSCALTitleKey is the title field within metadata
	OSCALTitleKey = "title"
)
