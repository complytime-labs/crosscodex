package tlsconfig

import (
	"crypto/tls"
	"fmt"
	"strings"
)

// VerifyFIPSBuild checks whether the running binary was built with BoringCrypto.
// Returns FIPSStatus with Enabled=true and Provider="BoringCrypto" when the
// binary was built with GOEXPERIMENT=boringcrypto.
// Returns ErrFIPSNotEnabled when BoringCrypto is not available.
func VerifyFIPSBuild() (*FIPSStatus, error) {
	var r Resolver
	return r.VerifyFIPSBuild()
}

// VerifyFIPSBuild is the method form of the package-level function.
func (r Resolver) VerifyFIPSBuild() (*FIPSStatus, error) {
	if boringEnabled() {
		return &FIPSStatus{
			Enabled:  true,
			Provider: "BoringCrypto",
		}, nil
	}
	return &FIPSStatus{
		Enabled:  false,
		Provider: "",
	}, ErrFIPSNotEnabled
}

// fipsCipherSuites returns the IDs of all non-insecure cipher suites whose
// names contain "GCM". This is the FIPS base filter: only AES-GCM suites
// are permitted.
func fipsCipherSuites() []uint16 {
	var ids []uint16
	for _, cs := range tls.CipherSuites() {
		if strings.Contains(cs.Name, "GCM") {
			ids = append(ids, cs.ID)
		}
	}
	return ids
}

// filterCiphers applies the cipher pipeline:
// 1. Start with base set (all non-insecure suites, or FIPS-filtered set)
// 2. If allow is non-empty, keep only suites matching any allow substring
// 3. If deny is non-empty, remove suites matching any deny substring
//
// Returns ErrNoCiphersAvailable if no suites survive the pipeline.
func filterCiphers(base []uint16, allow, deny []string) ([]uint16, error) {
	// Build a name lookup from tls.CipherSuites()
	allSuites := tls.CipherSuites()
	nameByID := make(map[uint16]string, len(allSuites))
	for _, cs := range allSuites {
		nameByID[cs.ID] = cs.Name
	}

	result := make([]uint16, len(base))
	copy(result, base)

	// Allow filter: keep only suites whose name substring-matches any entry
	if len(allow) > 0 {
		var filtered []uint16
		for _, id := range result {
			name := nameByID[id]
			for _, pattern := range allow {
				if strings.Contains(name, pattern) {
					filtered = append(filtered, id)
					break
				}
			}
		}
		result = filtered
	}

	// Deny filter: remove suites whose name substring-matches any entry
	if len(deny) > 0 {
		var filtered []uint16
		for _, id := range result {
			name := nameByID[id]
			denied := false
			for _, pattern := range deny {
				if strings.Contains(name, pattern) {
					denied = true
					break
				}
			}
			if !denied {
				filtered = append(filtered, id)
			}
		}
		result = filtered
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("%w: allow=%v deny=%v", ErrNoCiphersAvailable, allow, deny)
	}

	return result, nil
}

// allNonInsecureCipherIDs returns the IDs of all suites from tls.CipherSuites().
func allNonInsecureCipherIDs() []uint16 {
	suites := tls.CipherSuites()
	ids := make([]uint16, len(suites))
	for i, cs := range suites {
		ids[i] = cs.ID
	}
	return ids
}
