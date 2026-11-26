package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

const asvsFixture = "../../internal/catalog/testdata/ingestion/owasp-asvs-5.0.0-excerpt.json"

var _ = Describe("catalog content-hash", func() {
	writeTemp := func(content []byte) string {
		path := filepath.Join(GinkgoT().TempDir(), "catalog.json")
		Expect(os.WriteFile(path, content, 0o600)).To(Succeed())
		return path
	}

	// The v1 hash literal is pinned once, in the adapter's specs.
	asvsHash := func() string {
		GinkgoHelper()
		data, err := os.ReadFile(asvsFixture)
		Expect(err).NotTo(HaveOccurred())
		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		return meta.ContentHash
	}

	It("prints the ingestion content hash in text", func() {
		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", asvsFixture)
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("content_hash: " + asvsHash() + " (ingestion)"))
	})

	It("prints the path, kind and hash as JSON for ingestion output", func() {
		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", "--json", asvsFixture)
		Expect(err).NotTo(HaveOccurred())
		var got map[string]string
		Expect(json.Unmarshal([]byte(stdout), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]string{
			"path": asvsFixture, "kind": "ingestion", "content_hash": asvsHash(),
		}))
	})

	It("hashes the bytes of an OSCAL catalog", func() {
		content := []byte(`{"catalog":{"uuid":"u","metadata":{"title":"t"}}}`)
		path := writeTemp(content)
		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", "--json", path)
		Expect(err).NotTo(HaveOccurred())
		var got map[string]string
		Expect(json.Unmarshal([]byte(stdout), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]string{
			"path": path, "kind": "bytes", "content_hash": storage.ContentHash(content),
		}))
	})

	It("hashes the bytes when an OSCAL catalog also looks like ingestion output", func() {
		content := []byte(`{"catalog":{"uuid":"u","metadata":{"title":"t"}},"schema_version":"1","elements":[]}`)
		path := writeTemp(content)
		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", "--json", path)
		Expect(err).NotTo(HaveOccurred())
		var got map[string]string
		Expect(json.Unmarshal([]byte(stdout), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]string{
			"path": path, "kind": "bytes", "content_hash": storage.ContentHash(content),
		}))
	})

	DescribeTable("hashes the bytes of a file that is not ingestion output",
		func(content string) {
			path := writeTemp([]byte(content))
			stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", "--json", path)
			Expect(err).NotTo(HaveOccurred())
			var got map[string]string
			Expect(json.Unmarshal([]byte(stdout), &got)).To(Succeed())
			Expect(got).To(Equal(map[string]string{
				"path": path, "kind": "bytes", "content_hash": storage.ContentHash([]byte(content)),
			}))
		},
		Entry("plain text", "not json at all"),
		Entry("JSON without elements", `{"schema_version":"1.3"}`),
	)

	It("rejects ingestion output with a malformed mapping sha256 and prints nothing", func() {
		raw, err := os.ReadFile(asvsFixture)
		Expect(err).NotTo(HaveOccurred())
		var doc map[string]any
		Expect(json.Unmarshal(raw, &doc)).To(Succeed())
		source := doc["metadata"].(map[string]any)["source"].(map[string]any)
		source["mapping"] = map[string]any{"id": "m", "sha256": "not-a-hash", "mapping_version": 1}
		bad, err := json.Marshal(doc)
		Expect(err).NotTo(HaveOccurred())

		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", writeTemp(bad))
		Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
		Expect(stdout).To(BeEmpty())
	})

	It("names the path of a missing file", func() {
		missing := filepath.Join(GinkgoT().TempDir(), "absent.json")
		_, _, err := execute(newRootCmd(), "catalog", "content-hash", missing)
		Expect(err).To(MatchError(ContainSubstring(missing)))
	})

	DescribeTable("rejects a wrong argument count with usage guidance",
		func(args ...string) {
			_, _, err := execute(newRootCmd(), append([]string{"catalog", "content-hash"}, args...)...)
			Expect(err).To(MatchError(ContainSubstring("requires a file path")))
			Expect(err).To(MatchError(ContainSubstring("crosscodex catalog content-hash <file>")))
		},
		Entry("with no arguments"),
		Entry("with two arguments", "a.json", "b.json"),
	)

	It("explains where the hash shows up and when ingestion applies", func() {
		stdout, _, err := execute(newRootCmd(), "catalog", "content-hash", "--help")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout).To(ContainSubstring("catalog inspect"))
	})
})
