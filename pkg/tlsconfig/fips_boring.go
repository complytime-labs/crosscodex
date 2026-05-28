//go:build boringcrypto

package tlsconfig

import "crypto/boring"

// boringEnabled reports whether BoringCrypto is available and active.
// This file is only compiled when GOEXPERIMENT=boringcrypto is set.
func boringEnabled() bool {
	return boring.Enabled()
}
