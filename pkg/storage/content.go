package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ContentHash returns the hex-encoded SHA-256 digest of data.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ContentKey returns the storage key for a content-addressed attestation bundle.
// The returned key is relative — the tenant prefix is handled by the Provider.
func ContentKey(data []byte) string {
	return fmt.Sprintf("attestation/%s.json", ContentHash(data))
}
