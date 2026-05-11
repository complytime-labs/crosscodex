package tlsconfig

// FIPSStatus represents the FIPS mode status.
type FIPSStatus struct {
	Enabled  bool   // Whether FIPS mode is active
	Provider string // Cryptographic provider name
	Version  string // Provider version
}

// CertificateInfo holds parsed certificate information.
type CertificateInfo struct {
	Subject      string   // Certificate subject DN
	Issuer       string   // Certificate issuer DN
	NotBefore    int64    // Validity start (Unix timestamp)
	NotAfter     int64    // Validity end (Unix timestamp)
	SerialNumber string   // Certificate serial number
	DNSNames     []string // Subject alternative names (DNS)
}
