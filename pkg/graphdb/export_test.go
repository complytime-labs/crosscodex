package graphdb

// export_test.go exposes unexported functions to the external test package
// (graphdb_test) via the standard Go bridge-file pattern. This file is in
// package graphdb but its name ends in _test.go, so it is compiled only
// during testing.

var ParseAGVertex = parseAGVertex
var ParseAGEdge = parseAGEdge
var ParseAGPath = parseAGPath
var SplitAGPathElements = splitAGPathElements
var StripSuffix = stripSuffix
var EscapeCypher = escapeCypher
var CypherValue = cypherValue
var NodeToAGProperties = nodeToAGProperties
var EdgeToAGProperties = edgeToAGProperties
var GraphName = graphName

// TelemetryFields exposes telemetry instrument state for test assertions.
type TelemetryFields struct {
	HasTracer       bool
	HasMeter        bool
	HasQueryCounter bool
	HasQueryLatency bool
}

// ExportTelemetryFields returns telemetry field presence for assertions.
func ExportTelemetryFields(g GraphDB) TelemetryFields {
	c, ok := g.(*ageClient)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:       c.tracer != nil,
		HasMeter:        c.meter != nil,
		HasQueryCounter: c.queryCounter != nil,
		HasQueryLatency: c.queryLatency != nil,
	}
}
