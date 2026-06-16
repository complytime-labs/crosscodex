package attestation

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// FileKeyProvider loads ECDSA P-256 signing/verification keys from PEM files.
type FileKeyProvider struct {
	PrivateKeyPath string
	PublicKeyPath  string
}

// SigningKey reads and parses the ECDSA private key from PrivateKeyPath.
func (f *FileKeyProvider) SigningKey(_ context.Context) (crypto.Signer, error) {
	data, err := os.ReadFile(f.PrivateKeyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("signing key file %s: %w: %w", f.PrivateKeyPath, ErrKeyNotFound, err)
		}
		return nil, fmt.Errorf("signing key file %s: %w: %w", f.PrivateKeyPath, ErrKeyNotFound, err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse signing key %s: %w: no PEM block found", f.PrivateKeyPath, ErrKeyLoadFailed)
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key %s: %w: %w", f.PrivateKeyPath, ErrKeyLoadFailed, err)
	}

	return key, nil
}

// VerificationKey reads and parses the public key from PublicKeyPath.
func (f *FileKeyProvider) VerificationKey(_ context.Context) (crypto.PublicKey, error) {
	data, err := os.ReadFile(f.PublicKeyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("verification key file %s: %w: %w", f.PublicKeyPath, ErrKeyNotFound, err)
		}
		return nil, fmt.Errorf("verification key file %s: %w: %w", f.PublicKeyPath, ErrKeyNotFound, err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("parse verification key %s: %w: no PEM block found", f.PublicKeyPath, ErrKeyLoadFailed)
	}

	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse verification key %s: %w: %w", f.PublicKeyPath, ErrKeyLoadFailed, err)
	}

	return key, nil
}

// KeyID computes a deterministic key identifier from the public key.
// Returns the SHA-256 hex digest of the DER-encoded public key.
func (f *FileKeyProvider) KeyID(ctx context.Context) (string, error) {
	pub, err := f.VerificationKey(ctx)
	if err != nil {
		return "", err
	}

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key for key ID: %w: %w", ErrKeyLoadFailed, err)
	}

	hash := sha256.Sum256(der)
	return hex.EncodeToString(hash[:]), nil
}

// EphemeralKeyProvider generates an ECDSA P-256 key pair in memory.
// Used when no key paths are configured (embedded/dev mode).
// The key pair exists only for the lifetime of the provider instance.
type EphemeralKeyProvider struct {
	key   *ecdsa.PrivateKey
	keyID string
}

// NewEphemeralKeyProvider generates a new ECDSA P-256 key pair in memory.
func NewEphemeralKeyProvider() (*EphemeralKeyProvider, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal ephemeral public key: %w", err)
	}
	hash := sha256.Sum256(der)
	keyID := hex.EncodeToString(hash[:])

	return &EphemeralKeyProvider{key: key, keyID: keyID}, nil
}

// SigningKey returns the ephemeral private key.
func (e *EphemeralKeyProvider) SigningKey(_ context.Context) (crypto.Signer, error) {
	return e.key, nil
}

// VerificationKey returns the ephemeral public key.
func (e *EphemeralKeyProvider) VerificationKey(_ context.Context) (crypto.PublicKey, error) {
	return &e.key.PublicKey, nil
}

// KeyID returns the SHA-256 fingerprint of the ephemeral public key.
func (e *EphemeralKeyProvider) KeyID(_ context.Context) (string, error) {
	return e.keyID, nil
}
