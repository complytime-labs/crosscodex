package authn

import "crypto/x509"

// CertClaims exposes certClaims for BDD tests.
var CertClaims = certClaims

// GlobMatch exposes globMatch for BDD tests.
var GlobMatch = globMatch

// MatchFirst exposes matchFirst for BDD tests.
var MatchFirst = matchFirst

// MatchAny exposes matchAny for BDD tests.
var MatchAny = matchAny

// MatchAnyURI exposes matchAnyURI for BDD tests.
func MatchAnyURI(pattern string, cert *x509.Certificate) bool {
	return matchAnyURI(pattern, cert)
}

// TelemetryFields exposes telemetry state for test assertions.
type TelemetryFields struct {
	HasTracer      bool
	HasMeter       bool
	HasAuthCounter bool
	HasAuthLatency bool
}

// ExportTelemetryFields returns the telemetry configuration state of a Registry.
func ExportTelemetryFields(r *Registry) TelemetryFields {
	return TelemetryFields{
		HasTracer:      r.tracer != nil,
		HasMeter:       r.meter != nil,
		HasAuthCounter: r.authCounter != nil,
		HasAuthLatency: r.authLatency != nil,
	}
}
