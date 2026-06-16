package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
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

// JobAttestationKey returns the storage key for a job-structured attestation artifact.
// The returned key is relative — the tenant prefix is handled by the Provider.
//
// Examples:
//
//	JobAttestationKey("job-123", "layout.json")          → "jobs/job-123/attestation/layout.json"
//	JobAttestationKey("job-123", "ingestion.link.json")  → "jobs/job-123/attestation/ingestion.link.json"
//	JobAttestationKey("job-123", "input_manifest.sha256") → "jobs/job-123/attestation/input_manifest.sha256"
func JobAttestationKey(jobID, filename string) string {
	return path.Join("jobs", jobID, "attestation", filename)
}
