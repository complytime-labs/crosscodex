package crosswalk_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

const repoRoot = "../../.."

var _ = Describe("Shipped profiles", func() {
	profiles, err := filepath.Glob("profiles/*.yaml")
	if err != nil || len(profiles) == 0 {
		Fail("no shipped profiles found under profiles/*.yaml")
	}

	for _, path := range profiles {
		It("resolves every reference of "+filepath.Base(path), func() {
			p, err := crosswalk.LoadProfile(path)
			Expect(err).NotTo(HaveOccurred())

			for _, rel := range []string{p.Source, p.Target} {
				if _, err := os.Stat(filepath.Join(repoRoot, rel)); os.IsNotExist(err) {
					Skip("catalogs not fetched; run task fetch:oscal-docs")
				}
			}
			src, err := crosswalk.LoadCatalog(filepath.Join(repoRoot, p.Source))
			Expect(err).NotTo(HaveOccurred())
			tgt, err := crosswalk.LoadCatalog(filepath.Join(repoRoot, p.Target))
			Expect(err).NotTo(HaveOccurred())

			raw, err := crosswalk.ExtractReferences(p, repoRoot, src)
			Expect(err).NotTo(HaveOccurred())
			refs := crosswalk.ResolveReferences(raw, crosswalk.NewTargetIndex(tgt))

			Expect(refs.Resolved).To(HaveLen(declaredReferences(path)), "the # references: header of %s", path)
			Expect(refs.Unresolved).To(BeEmpty())
			Expect(refs.Ambiguous).To(BeEmpty())
			covered := map[string]bool{}
			for _, r := range refs.Resolved {
				covered[r.SourceID] = true
			}
			for _, c := range p.Controls {
				Expect(covered).To(HaveKey(c), "control %s has no reference", c)
			}
		})
	}
})

var referencesHeader = regexp.MustCompile(`(?m)^# references: (\d+)$`)

// declaredReferences reads the resolved reference count a profile states in
// its "# references: N" header line, so the profile stays the one source.
func declaredReferences(path string) int {
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	m := referencesHeader.FindSubmatch(raw)
	Expect(m).NotTo(BeNil(), "%s has no \"# references: N\" header line", path)
	n, err := strconv.Atoi(string(m[1]))
	Expect(err).NotTo(HaveOccurred())
	return n
}
