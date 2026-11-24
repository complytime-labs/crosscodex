package backend

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrUnresolvableURI marks an ingestion output URI that is not an object of
// the calling tenant in the configured storage backend. Callers classify with
// errors.Is; the message never names another tenant or a filesystem root.
var ErrUnresolvableURI = errors.New("unresolvable ingestion output URI")

// StorageBackend describes the configured object storage the ingestion
// service writes to, as needed to validate the URIs it returns.
type StorageBackend struct {
	// Kind is "local" (file:// URIs under Root) or "s3" (s3:// URIs in Bucket).
	Kind string
	// Root is the absolute directory that holds one subdirectory per tenant.
	// Used when Kind is "local".
	Root string
	// Bucket is the bucket that holds one key prefix per tenant. Used when
	// Kind is "s3".
	Bucket string
}

func rejectURI(reason string) error {
	return fmt.Errorf("%w: %s; the ingestion service must return a file:// or s3:// URI inside the configured storage backend for the calling tenant", ErrUnresolvableURI, reason)
}

// ResolveDocumentKey maps a service-supplied ingestion output URI to the
// tenant-relative key that the calling tenant's storage.Provider accepts
// (pkg/storage providers add the "<tenant>/" prefix or directory themselves).
//
// The URI is untrusted. It is accepted only when its scheme matches b.Kind,
// it lies in the configured root or bucket, its first segment is the tenant
// on ctx, and the remaining segments are non-empty and free of "." and "..".
// Percent-escapes are decoded exactly once, before the segment checks.
// Every rejection wraps ErrUnresolvableURI.
func ResolveDocumentKey(ctx context.Context, uri string, b StorageBackend) (string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", rejectURI("no tenant on the request context")
	}
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return "", rejectURI("the request tenant is malformed")
	}

	u, err := url.Parse(uri)
	if err != nil {
		return "", rejectURI("the URI is malformed")
	}
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", rejectURI("the URI carries credentials, a query or a fragment, or is not a hierarchical URI")
	}

	var rel string // path below the backend's root or bucket, still holding the tenant segment
	switch b.Kind {
	case "local":
		if !path.IsAbs(b.Root) {
			return "", errors.New("local storage root must be an absolute path; fix the storage configuration")
		}
		if u.Scheme != "file" {
			return "", rejectURI("the URI scheme does not match the local storage backend")
		}
		if u.Host != "" {
			return "", rejectURI("a file:// URI must not name a host")
		}
		prefix := strings.TrimSuffix(path.Clean(b.Root), "/") + "/"
		if !strings.HasPrefix(u.Path, prefix) {
			return "", rejectURI("the file is outside the configured storage root")
		}
		rel = strings.TrimPrefix(u.Path, prefix)
	case "s3":
		if b.Bucket == "" {
			return "", errors.New("s3 storage bucket must be set; fix the storage configuration")
		}
		if u.Scheme != "s3" {
			return "", rejectURI("the URI scheme does not match the s3 storage backend")
		}
		if u.Host != b.Bucket {
			return "", rejectURI("the URI names a bucket other than the configured one")
		}
		rel = strings.TrimPrefix(u.Path, "/")
	default:
		return "", rejectURI("the storage backend kind is not supported")
	}

	segments := strings.Split(rel, "/")
	for _, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", rejectURI("the path has an empty, '.' or '..' segment")
		}
	}
	if segments[0] != tenantID {
		return "", rejectURI("the object does not belong to the calling tenant")
	}
	if len(segments) < 2 {
		return "", rejectURI("the URI names no object inside the tenant")
	}
	key := strings.Join(segments[1:], "/")
	if err := storage.ValidateKey(key); err != nil {
		return "", rejectURI("the object key is not valid for storage")
	}
	return key, nil
}
