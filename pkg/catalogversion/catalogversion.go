// Package catalogversion renders a catalog document version for humans. It is
// the one formatter of "slug @ label (date · hash6)", shared by catalog import
// and listing (internal/catalog), job snapshots (internal/pipeline), exports
// and job reports, so every surface names a version the same way.
package catalogversion

import (
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// hashPrefixLen is the number of leading characters of a content hash shown
// to humans.
const hashPrefixLen = 6

// DateAndHash renders importedAt as a UTC date and the first six characters
// of contentHash: "2006-01-02 · 012345". A shorter hash is used whole.
func DateAndHash(importedAt time.Time, contentHash string) string {
	prefix := contentHash
	if len(prefix) > hashPrefixLen {
		prefix = prefix[:hashPrefixLen]
	}
	return importedAt.UTC().Format("2006-01-02") + " · " + prefix
}

// Format renders a version as "slug @ label (date · hash6)". The label is
// untrusted input validated at import; callers printing to a terminal still
// escape the result.
func Format(slug, label string, importedAt time.Time, contentHash string) string {
	return fmt.Sprintf("%s @ %s (%s)", slug, label, DateAndHash(importedAt, contentHash))
}

// OfCatalog renders cat's version from the fields CatalogService returns
// (document_slug, version_label, audit.created_at and the "content_hash"
// lineage entry). It returns "" for a catalog outside any document.
func OfCatalog(cat *pb.Catalog) string {
	if cat.GetDocumentSlug() == "" {
		return ""
	}
	return Format(
		cat.GetDocumentSlug(),
		cat.GetVersionLabel(),
		cat.GetAudit().GetCreatedAt().AsTime(),
		cat.GetProvenance().GetLineageMetadata()["content_hash"],
	)
}
