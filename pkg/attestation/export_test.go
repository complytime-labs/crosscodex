package attestation

import (
	in_toto "github.com/in-toto/in-toto-golang/in_toto"
)

// ArtifactsToHashObj exports the unexported artifactsToHashObj for BDD tests.
var ArtifactsToHashObj = artifactsToHashObj

// HashObjToArtifacts exports the unexported hashObjToArtifacts for BDD tests.
// Uses map[string]map[string]string to avoid exposing in-toto types in the test API.
func HashObjToArtifacts(hashObjs map[string]map[string]string) []Artifact {
	itoHashObjs := make(map[string]in_toto.HashObj, len(hashObjs))
	for k, v := range hashObjs {
		itoHashObjs[k] = in_toto.HashObj(v)
	}
	return hashObjToArtifacts(itoHashObjs)
}

// TelemetryFields exposes telemetry wiring state for BDD tests.
type TelemetryFields struct {
	HasTracer    bool
	HasMeter     bool
	HasOpCounter bool
	HasOpLatency bool
}

// ExportTelemetryFields extracts telemetry state from a Generator.
func ExportTelemetryFields(g Generator) TelemetryFields {
	gen, ok := g.(*generator)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:    gen.tracer != nil,
		HasMeter:     gen.meter != nil,
		HasOpCounter: gen.opCounter != nil,
		HasOpLatency: gen.opLatency != nil,
	}
}
