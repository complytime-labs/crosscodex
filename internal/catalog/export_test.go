package catalog

// Export unexported functions for property and fuzz testing.

var (
	ExportIsOSCALJSON          = isOSCALJSON
	ExportValidateItems        = validateItems
	ExportGenerateCatalogID    = generateCatalogID
	ExportMergeResults         = mergeResults
	ExportCatalogRecordToProto = catalogRecordToProto
	ExportControlRecordToProto = controlRecordToProto
)

// Type aliases for test accessibility.
type ExportControlRecord = ControlRecord
type ExportCatalogRecord = CatalogRecord
