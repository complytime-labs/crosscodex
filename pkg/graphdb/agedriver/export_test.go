package agedriver

import "github.com/complytime-labs/crosscodex/pkg/graphdb"

var ParseAGVertex = parseAGVertex
var ParseAGEdge = parseAGEdge
var ParseAGPath = parseAGPath
var SplitAGPathElements = splitAGPathElements
var StripSuffix = stripSuffix
var EscapeCypher = escapeCypher
var ExportCypherDollarTag = cypherDollarTag
var CypherValue = cypherValue
var NodeToAGProperties = nodeToAGProperties
var NodeUpsertProperties = nodeUpsertProperties
var EdgeToAGProperties = edgeToAGProperties
var GraphName = graphName
var ParseQueryValue = parseQueryValue
var SubstituteParams = substituteParams
var ClassifyErr = classifyErr
var CreateLabelEdges = createLabelEdges
var CypherStringList = cypherStringList
var AdvisoryKey = advisoryKey
var MarkOutcome = markOutcome

type TelemetryFields struct {
	HasTracer       bool
	HasMeter        bool
	HasQueryCounter bool
	HasQueryLatency bool
}

func ExportTelemetryFields(g graphdb.GraphDB) TelemetryFields {
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
