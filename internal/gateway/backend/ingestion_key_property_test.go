package backend

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Describe("ResolveDocumentKey", func() {
		const bucket = "crosscodex-objects"
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		if err != nil {
			panic(err)
		}
		s3 := StorageBackend{Kind: "s3", Bucket: bucket}
		local := StorageBackend{Kind: "local", Root: "/srv/objects"}

		// pathChars includes dots, slashes, percent escapes and a backslash so
		// the generator reaches traversal and empty-segment spellings.
		pathGen := rapid.StringMatching(`[a-z0-9./%\\-]{0,40}`)

		It("only accepts keys the storage provider also accepts, free of dot, dot-dot and empty segments", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				suffix := pathGen.Draw(t, "suffix")
				for _, tc := range []struct {
					uri string
					b   StorageBackend
				}{
					{"s3://" + bucket + "/tenant-a/" + suffix, s3},
					{"file:///srv/objects/tenant-a/" + suffix, local},
				} {
					key, err := ResolveDocumentKey(ctx, tc.uri, tc.b)
					if err != nil {
						continue
					}
					if verr := storage.ValidateKey(key); verr != nil {
						t.Fatalf("accepted key %q rejected by storage.ValidateKey: %v", key, verr)
					}
					for _, seg := range strings.Split(key, "/") {
						if seg == "" || seg == "." || seg == ".." {
							t.Fatalf("accepted key %q has segment %q (uri %q)", key, seg, tc.uri)
						}
					}
				}
			})
		})

		It("never accepts a URI naming a different tenant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				other := rapid.StringMatching(`[a-z][a-z0-9-]{1,10}[a-z0-9]`).Draw(t, "other")
				if other == "tenant-a" {
					t.Skip()
				}
				suffix := pathGen.Draw(t, "suffix")
				if key, err := ResolveDocumentKey(ctx, "s3://"+bucket+"/"+other+"/"+suffix, s3); err == nil {
					t.Fatalf("accepted cross-tenant URI as key %q", key)
				}
			})
		})
	})
})
