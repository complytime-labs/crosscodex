package backend

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("ResolveDocumentKey", func() {
	const (
		callerTenant = "tenant-a"
		otherTenant  = "tenant-b-secret"
		root         = "/srv/crosscodex/objects"
		bucket       = "crosscodex-objects"
	)
	local := StorageBackend{Kind: "local", Root: root}
	s3 := StorageBackend{Kind: "s3", Bucket: bucket}

	ctxFor := func(id string) context.Context {
		ctx, err := tenant.WithTenant(context.Background(), id)
		Expect(err).NotTo(HaveOccurred())
		return ctx
	}

	DescribeTable("accepts a URI inside the caller's tenant",
		func(uri string, b StorageBackend, want string) {
			key, err := ResolveDocumentKey(ctxFor(callerTenant), uri, b)
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(want))
		},
		Entry("file URI under root", "file://"+root+"/tenant-a/documents/d1/out.json", local, "documents/d1/out.json"),
		Entry("s3 URI in configured bucket", "s3://"+bucket+"/tenant-a/documents/d1/out.json", s3, "documents/d1/out.json"),
		Entry("percent-encoded space in a segment", "file://"+root+"/tenant-a/documents/d%201/out.json", local, "documents/d 1/out.json"),
		// Escapes are decoded once: %252e%252e becomes the literal segment
		// "%2e%2e", a directory name, never "..".
		Entry("double-encoded dot-dot kept as the literal segment %2e%2e",
			"file://"+root+"/tenant-a/%252e%252e/out.json", local, "%2e%2e/out.json"),
	)

	DescribeTable("rejects the URI and leaks neither tenant nor root",
		func(uri string, b StorageBackend, ctx context.Context) {
			key, err := ResolveDocumentKey(ctx, uri, b)
			Expect(err).To(MatchError(ErrUnresolvableURI))
			Expect(key).To(BeEmpty())
			Expect(err.Error()).NotTo(ContainSubstring(otherTenant))
			Expect(err.Error()).NotTo(ContainSubstring(root))
			Expect(err.Error()).To(ContainSubstring("ingestion"), "message must say what to fix")
		},
		Entry("http scheme", "http://example.com/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("gopher scheme", "gopher://example.com/tenant-a/documents/d1/f", s3, ctxFor(callerTenant)),
		Entry("no scheme", root+"/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("s3 URI on local backend", "s3://"+bucket+"/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("file URI on s3 backend", "file://"+root+"/tenant-a/documents/d1/f", s3, ctxFor(callerTenant)),
		Entry("other tenant on file", "file://"+root+"/"+otherTenant+"/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("other tenant on s3", "s3://"+bucket+"/"+otherTenant+"/documents/d1/f", s3, ctxFor(callerTenant)),
		Entry("dot-dot into other tenant", "file://"+root+"/tenant-a/../"+otherTenant+"/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("dot-dot in s3 key", "s3://"+bucket+"/tenant-a/documents/../../"+otherTenant+"/f", s3, ctxFor(callerTenant)),
		Entry("encoded dot-dot", "file://"+root+"/tenant-a/%2e%2e/"+otherTenant+"/f", local, ctxFor(callerTenant)),
		Entry("encoded dot-dot on s3", "s3://"+bucket+"/tenant-a/%2E%2E/"+otherTenant+"/f", s3, ctxFor(callerTenant)),
		Entry("encoded slash hiding dot-dot", "file://"+root+"/tenant-a/..%2f"+otherTenant+"/f", local, ctxFor(callerTenant)),
		Entry("empty segment", "file://"+root+"/tenant-a//documents/d1/f", local, ctxFor(callerTenant)),
		Entry("empty segment on s3", "s3://"+bucket+"/tenant-a/documents//f", s3, ctxFor(callerTenant)),
		Entry("dot segment", "file://"+root+"/tenant-a/./documents/f", local, ctxFor(callerTenant)),
		Entry("trailing slash", "file://"+root+"/tenant-a/documents/d1/", local, ctxFor(callerTenant)),
		Entry("absolute escape", "file:///etc/passwd", local, ctxFor(callerTenant)),
		Entry("path outside root", "file:///srv/other/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("root prefix sibling", "file://"+root+"-evil/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("file URI with remote host", "file://evil.example"+root+"/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
		Entry("wrong bucket", "s3://other-bucket/tenant-a/documents/d1/f", s3, ctxFor(callerTenant)),
		Entry("userinfo in s3 URI", "s3://user:pw@"+bucket+"/tenant-a/documents/d1/f", s3, ctxFor(callerTenant)),
		Entry("query string", "s3://"+bucket+"/tenant-a/documents/d1/f?versionId=1", s3, ctxFor(callerTenant)),
		Entry("backslash in key", "s3://"+bucket+"/tenant-a/documents/d1%5Cf", s3, ctxFor(callerTenant)),
		Entry("NUL byte in key", "s3://"+bucket+"/tenant-a/documents/d1%00f", s3, ctxFor(callerTenant)),
		Entry("tenant only, no object", "s3://"+bucket+"/tenant-a", s3, ctxFor(callerTenant)),
		Entry("tenant dir only", "s3://"+bucket+"/tenant-a/", s3, ctxFor(callerTenant)),
		Entry("empty URI", "", local, ctxFor(callerTenant)),
		Entry("malformed URI", "s3://%zz/tenant-a/f", s3, ctxFor(callerTenant)),
		Entry("unknown backend kind", "s3://"+bucket+"/tenant-a/documents/d1/f", StorageBackend{Kind: "ftp"}, ctxFor(callerTenant)),
		Entry("context without tenant", "s3://"+bucket+"/tenant-a/documents/d1/f", s3, context.Background()),
		Entry("valid ctx tenant other than the URI's", "s3://"+bucket+"/tenant-a/documents/d1/f", s3, ctxFor("tenant-c")),
		Entry("fragment", "s3://"+bucket+"/tenant-a/documents/d1/f#x", s3, ctxFor(callerTenant)),
		Entry("opaque file URI", "file:relative/tenant-a/documents/d1/f", local, ctxFor(callerTenant)),
	)

	DescribeTable("rejects a misconfigured backend, naming the setting to fix",
		func(uri string, b StorageBackend, want string) {
			key, err := ResolveDocumentKey(ctxFor(callerTenant), uri, b)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err).To(MatchError(ContainSubstring("fix the storage configuration")))
			Expect(key).To(BeEmpty())
		},
		Entry("relative local root", "file://relative/tenant-a/documents/d1/f",
			StorageBackend{Kind: "local", Root: "relative"}, "absolute"),
		Entry("empty s3 bucket", "s3:///tenant-a/documents/d1/f",
			StorageBackend{Kind: "s3"}, "bucket must be set"),
	)

	Describe("round trip through a real provider", func() {
		It("reads the file written at a file:// URI via the local provider", func() {
			dir := GinkgoT().TempDir()
			prov, err := storage.NewLocal(dir, callerTenant)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(prov.Close)
			Expect(os.MkdirAll(filepath.Join(dir, callerTenant, "documents", "d1"), 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, callerTenant, "documents", "d1", "out.json"), []byte("payload"), 0o600)).To(Succeed())

			uri := "file://" + filepath.Join(dir, callerTenant, "documents", "d1", "out.json")
			key, err := ResolveDocumentKey(ctxFor(callerTenant), uri, StorageBackend{Kind: "local", Root: dir})
			Expect(err).NotTo(HaveOccurred())

			rc, err := prov.Get(context.Background(), key)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(rc.Close)
			data, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("payload"))
		})

		It("reads the object written at an s3:// URI via the tenant's provider fake", func() {
			prov := &storagetest.Fake{}
			Expect(prov.Put(context.Background(), "documents/d1/out.json", strings.NewReader("payload"))).To(Succeed())

			key, err := ResolveDocumentKey(ctxFor(callerTenant), "s3://"+bucket+"/tenant-a/documents/d1/out.json", s3)
			Expect(err).NotTo(HaveOccurred())

			rc, err := prov.Get(context.Background(), key)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(rc.Close)
			data, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("payload"))
		})
	})
})
