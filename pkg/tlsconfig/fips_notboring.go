//go:build !boringcrypto

package tlsconfig

// boringEnabled reports whether BoringCrypto is available and active.
// In standard builds without GOEXPERIMENT=boringcrypto, this always returns false.
func boringEnabled() bool {
	return false
}
