// Package retention implements data lifecycle policy resolution, candidate
// collection, and expiry evaluation for the crosscodex retention engine.
package retention

import "time"

// DataClass identifies the category of data subject to retention policy.
type DataClass string

const (
	ClassJobResults  DataClass = "job_results"
	ClassCatalogs    DataClass = "catalogs"
	ClassEmbeddings  DataClass = "embeddings"
	ClassAttestation DataClass = "attestation"
)

// Store identifies the storage backend that holds a candidate.
type Store string

const (
	StorePostgres Store = "postgres"
	StoreObject   Store = "object"
)

// Candidate is a single data item eligible for retention evaluation.
type Candidate struct {
	Store     Store
	Class     DataClass
	ID        string
	TenantID  string
	CreatedAt time.Time
	Size      int64
	ObjectKey string
}
