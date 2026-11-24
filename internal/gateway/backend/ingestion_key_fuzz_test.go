package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// FuzzResolveDocumentKey checks that no input panics and that every accepted
// key is tenant-relative and traversal-free for both backend kinds.
func FuzzResolveDocumentKey(f *testing.F) {
	for _, seed := range []string{
		"s3://crosscodex-objects/tenant-a/documents/d1/out.json",      // valid s3
		"file:///srv/objects/tenant-a/documents/d1/out.json",          // valid file
		"http://example.com/tenant-a/documents/d1/out.json",           // invalid scheme
		"s3://crosscodex-objects/tenant-a",                            // boundary: no object
		"file:///srv/objects/tenant-a/%2e%2e/tenant-b/x",              // attack: encoded traversal
		"s3://crosscodex-objects/tenant-b/documents/d1/out.json",      // attack: other tenant
		"s3://crosscodex-objects/tenant-a/documents//d1\x00/out.json", // attack: empty segment, NUL
		"file://evil/srv/objects/tenant-a/x",                          // attack: remote host
	} {
		f.Add(seed)
	}
	ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
	if err != nil {
		f.Fatal(err)
	}
	backends := []StorageBackend{
		{Kind: "s3", Bucket: "crosscodex-objects"},
		{Kind: "local", Root: "/srv/objects"},
	}
	f.Fuzz(func(t *testing.T, uri string) {
		for _, b := range backends {
			key, err := ResolveDocumentKey(ctx, uri, b)
			if err != nil {
				continue
			}
			if verr := storage.ValidateKey(key); verr != nil {
				t.Fatalf("accepted key %q fails storage.ValidateKey: %v", key, verr)
			}
			for _, seg := range strings.Split(key, "/") {
				if seg == "" || seg == "." || seg == ".." {
					t.Fatalf("accepted key %q has segment %q", key, seg)
				}
			}
		}
	})
}
