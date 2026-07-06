package vectordb

// ParseVectorString exports parseVectorString for use by the external
// test package (vectordb_test). This follows the standard Go convention
// of export_test.go files that live in the internal package but expose
// unexported symbols solely for testing.
var ParseVectorString = parseVectorString

// VectorToString exports vectorToString for property-test roundtrip
// assertions in the external test package.
var VectorToString = vectorToString

// TelemetryFields holds the telemetry state of a PgVectorStore,
// exported solely for assertions in the external test package.
type TelemetryFields struct {
	HasTracer        bool
	HasMeter         bool
	HasSearchCounter bool
	HasSearchLatency bool
	HasStoreCounter  bool
	HasStoreLatency  bool
}

// GetTelemetryFields returns the telemetry initialisation state of
// a PgVectorStore so the external test package can verify that
// WithTelemetry populates (or omits) every instrument.
func (db *PgVectorStore) GetTelemetryFields() TelemetryFields {
	return TelemetryFields{
		HasTracer:        db.tracer != nil,
		HasMeter:         db.meter != nil,
		HasSearchCounter: db.searchCounter != nil,
		HasSearchLatency: db.searchLatency != nil,
		HasStoreCounter:  db.storeCounter != nil,
		HasStoreLatency:  db.storeLatency != nil,
	}
}
