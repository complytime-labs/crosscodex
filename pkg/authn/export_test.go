package authn

import "crypto/x509"

// MatchCert exposes matchCert for BDD tests in the authn_test package.
var MatchCert = matchCert

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
