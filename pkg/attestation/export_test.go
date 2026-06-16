package attestation

import (
	"crypto"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
)

// ValidateFIPSKey exports the FIPS key validation function for testing.
var ValidateFIPSKey = func(signer crypto.Signer) error { return validateFIPSKey(signer) }

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

// TelemetryFields exposes generator configuration for testing.
type TelemetryFields struct {
	HasTracer         bool
	HasMeter          bool
	HasOpCounter      bool
	HasOpLatency      bool
	FIPSMode          bool
	IncludeByProducts bool
}

// ExportTelemetryFields returns the generator's configuration state.
func ExportTelemetryFields(g Generator) TelemetryFields {
	gen, ok := g.(*generator)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:         gen.tracer != nil,
		HasMeter:          gen.meter != nil,
		HasOpCounter:      gen.opCounter != nil,
		HasOpLatency:      gen.opLatency != nil,
		FIPSMode:          gen.fipsMode,
		IncludeByProducts: gen.includeByProducts,
	}
}
