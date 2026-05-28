package tlsconfig

// Resolver holds no state. Methods are exported for consumers who want to hold
// a reference rather than calling package-level functions. A zero-value Resolver
// is ready to use.
type Resolver struct{}

// FIPSStatus reports whether the binary was built with BoringCrypto.
type FIPSStatus struct {
	Enabled  bool   // Whether BoringCrypto is linked
	Provider string // "BoringCrypto" or ""
}
