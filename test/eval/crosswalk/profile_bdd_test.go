package crosswalk_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// profileYAML builds a profile document; body is the text after the header.
func profileYAML(body string) string {
	return "name: p\nsource: s.json\ntarget: t.json\n" + body
}

const validLinks = "controls: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource:\n      title: '^T$'\n"

func writeProfile(content string) string {
	path := filepath.Join(GinkgoT().TempDir(), "p.yaml")
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
	return path
}

var _ = Describe("LoadProfile", func() {
	It("loads a valid profile, keeps paths as written and records the file digest", func() {
		const path = "testdata/profiles/links_in_text.yaml"
		p, err := crosswalk.LoadProfile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.Name).To(Equal("synth-links-in-text"))
		Expect(p.Source).To(Equal("test/eval/crosswalk/testdata/source_a.json"))
		Expect(p.Controls).To(Equal([]string{"s-1", "s-2"}))
		Expect(p.References.OSCALLinks.Rel).To(Equal("x_ref"))
		Expect(p.References.OSCALLinks.Separator).To(Equal(","))
		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		sum := sha256.Sum256(raw)
		Expect(p.Digest).To(Equal(hex.EncodeToString(sum[:])))
	})

	It("defaults identifier to link_text and separator to a comma", func() {
		p, err := crosswalk.LoadProfile(writeProfile(profileYAML(validLinks)))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.References.OSCALLinks.Identifier).To(Equal("link_text"))
		Expect(p.References.OSCALLinks.Separator).To(Equal(","))
	})

	DescribeTable("rejects an invalid profile, naming the field",
		func(content, field string) {
			_, err := crosswalk.LoadProfile(writeProfile(content))
			Expect(err).To(MatchError(ContainSubstring(field)))
		},
		Entry("unknown top-level key", profileYAML(validLinks+"extra: 1\n"), "extra"),
		Entry("unknown nested key", profileYAML("controls: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    ressource: {title: a}\n"), "ressource"),
		Entry("missing name", "source: s\ntarget: t\n"+validLinks, "name"),
		Entry("missing source", "name: p\ntarget: t\n"+validLinks, "source"),
		Entry("missing target", "name: p\nsource: s\n"+validLinks, "target"),
		Entry("both oscal_links and csv", profileYAML(validLinks+"  csv: refs.csv\n"), "exactly one of references.oscal_links"),
		Entry("neither oscal_links nor csv", profileYAML("controls: [A-1]\nreferences: {}\n"), "exactly one of references.oscal_links"),
		Entry("empty controls", profileYAML("controls: []\nreferences:\n  csv: r.csv\n"), "controls"),
		Entry("more than 50 controls", profileYAML("controls: ["+strings.TrimSuffix(strings.Repeat("c, ", 51), ", ")+"]\nreferences:\n  csv: r.csv\n"), "50"),
		Entry("blank control", profileYAML("controls: [' ']\nreferences:\n  csv: r.csv\n"), "controls[0]"),
		Entry("duplicate control", profileYAML("controls: [A-1, A-1]\nreferences:\n  csv: r.csv\n"), "duplicate"),
		Entry("regex over 256 bytes", profileYAML("controls: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource: {title: '"+strings.Repeat("a", 257)+"'}\n"), "references.oscal_links.resource.title"),
		Entry("invalid regex", profileYAML("controls: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource: {rlink: '('}\n"), "references.oscal_links.resource.rlink"),
		Entry("unknown identifier mode", profileYAML(validLinks+"    identifier: other\n"), "identifier"),
		Entry("separator with resource_title", profileYAML(validLinks+"    identifier: resource_title\n    separator: ';'\n"), "separator"),
		Entry("selector with neither title nor rlink", profileYAML("controls: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource: {}\n"), "resource"),
		Entry("missing rel", profileYAML("controls: [A-1]\nreferences:\n  oscal_links:\n    resource: {title: a}\n"), "rel"),
		Entry("trailing second document", profileYAML(validLinks)+"---\nname: q\n", "single YAML document"),
	)

	It("rejects an unreadable path, naming it", func() {
		missing := filepath.Join(GinkgoT().TempDir(), "nope.yaml")
		_, err := crosswalk.LoadProfile(missing)
		Expect(err).To(MatchError(ContainSubstring(missing)))
	})

	It("rejects empty input", func() {
		_, err := crosswalk.LoadProfile(writeProfile(""))
		Expect(err).To(HaveOccurred())
	})
})
