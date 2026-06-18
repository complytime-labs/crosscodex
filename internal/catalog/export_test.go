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

// ExportServiceHasParser reports whether the Service has a parser set.
func ExportServiceHasParser(s *Service) bool { return s.parser != nil }

// ExportServiceHasStore reports whether the Service has a store set.
func ExportServiceHasStore(s *Service) bool { return s.store != nil }

// ExportServiceHasStorage reports whether the Service has a storage provider set.
func ExportServiceHasStorage(s *Service) bool { return s.storage != nil }
