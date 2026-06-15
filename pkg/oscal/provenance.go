package oscal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// Provenance tracks input metadata and hashes for audit trails.
type Provenance struct {
	SourceURI          string
	RetrievalTimestamp time.Time
	ContentHash        string
	ContentSize        int64
	Format             string
	OutputHash         string
	ExtractorName      string
	ExtractorVersion   string
}

// provenanceReader wraps an io.Reader and computes SHA-256 hash as data is read.
type provenanceReader struct {
	source     io.Reader
	provenance *Provenance
	hasher     *sha256Hash
	size       int64
}

// sha256Hash tracks hash state and total bytes read.
type sha256Hash struct {
	hash io.Writer
	size int64
}

// NewProvenance creates a Provenance tracker and returns a tee reader that computes content hash.
// After the returned reader is fully consumed, ContentHash and ContentSize are populated on the Provenance.
// Sets RetrievalTimestamp to now.
func NewProvenance(r io.Reader) (*Provenance, io.Reader, error) {
	p := &Provenance{
		RetrievalTimestamp: time.Now(),
	}

	hasher := &sha256Hash{
		hash: sha256.New(),
	}

	pr := &provenanceReader{
		source:     r,
		provenance: p,
		hasher:     hasher,
	}

	return p, pr, nil
}

// Read implements io.Reader and computes hash as data flows through.
func (pr *provenanceReader) Read(p []byte) (n int, err error) {
	n, err = pr.source.Read(p)
	if n > 0 {
		_, _ = pr.hasher.hash.Write(p[:n]) // hash.Write never returns an error
		pr.hasher.size += int64(n)
		pr.size += int64(n)
	}

	// On EOF, finalize the hash
	if err == io.EOF {
		pr.finalize()
	}

	return n, err
}

// finalize computes the final hash and updates the Provenance.
func (pr *provenanceReader) finalize() {
	// Get hash bytes
	var hashBytes []byte
	if h, ok := pr.hasher.hash.(interface{ Sum([]byte) []byte }); ok {
		hashBytes = h.Sum(nil)
	}

	// Set provenance fields
	pr.provenance.ContentHash = hex.EncodeToString(hashBytes)
	pr.provenance.ContentSize = pr.size
}

// SetExtractor sets the extractor name and version.
func (p *Provenance) SetExtractor(name, version string) {
	p.ExtractorName = name
	p.ExtractorVersion = version
}

// SetOutputHash computes SHA-256 of data and sets OutputHash.
func (p *Provenance) SetOutputHash(data []byte) {
	hash := sha256.Sum256(data)
	p.OutputHash = hex.EncodeToString(hash[:])
}

// String returns a human-readable representation of the Provenance.
func (p *Provenance) String() string {
	return fmt.Sprintf(
		"Provenance{SourceURI=%s, RetrievalTimestamp=%s, ContentHash=%s, ContentSize=%d, Format=%s, OutputHash=%s, ExtractorName=%s, ExtractorVersion=%s}",
		p.SourceURI,
		p.RetrievalTimestamp.Format(time.RFC3339),
		p.ContentHash,
		p.ContentSize,
		p.Format,
		p.OutputHash,
		p.ExtractorName,
		p.ExtractorVersion,
	)
}
