package crosswalk_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// FuzzLoadProfile feeds arbitrary bytes to the strict profile loader. On
// accepted input the documented invariants must hold.
func FuzzLoadProfile(f *testing.F) {
	f.Add([]byte("name: p\nsource: s\ntarget: t\ncontrols: [A-1]\nreferences:\n  csv: r.csv\n"))
	f.Add([]byte("name: p\nsource: s\ntarget: t\ncontrols: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource: {title: '^T$'}\n"))
	f.Add([]byte("name: p\nunknown: 1\n"))
	f.Add([]byte("controls: []\n"))
	f.Add([]byte("name: p\nsource: s\ntarget: t\ncontrols: [A-1]\nreferences:\n  oscal_links:\n    rel: x\n    resource: {rlink: '(((('}\n"))
	f.Add([]byte("&a [*a]"))
	f.Add([]byte{0xff, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "p.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := crosswalk.LoadProfile(path)
		if err != nil {
			return
		}
		if p.Name == "" || p.Source == "" || p.Target == "" {
			t.Fatalf("accepted profile with empty name/source/target: %+v", p)
		}
		if n := len(p.Controls); n < 1 || n > 50 {
			t.Fatalf("accepted %d controls", n)
		}
		if (p.References.OSCALLinks == nil) == (p.References.CSV == "") {
			t.Fatalf("accepted profile without exactly one reference source: %+v", p.References)
		}
		if l := p.References.OSCALLinks; l != nil {
			if l.Rel == "" || l.Resource.Title == "" && l.Resource.Rlink == "" {
				t.Fatalf("accepted incomplete oscal_links: %+v", l)
			}
		}
	})
}

// FuzzSplitLinkText checks that every identifier is non-empty and trimmed.
func FuzzSplitLinkText(f *testing.F) {
	f.Add("A-1, , A-2 ", ",")
	f.Add("", ",")
	f.Add(",,,", ",")
	f.Add("SA-15(7);AC-02", ";")
	f.Add("a , b", ",")
	f.Fuzz(func(t *testing.T, text, sep string) {
		if sep == "" {
			return
		}
		for _, id := range crosswalk.SplitLinkText(text, sep) {
			if id == "" || id != strings.TrimSpace(id) {
				t.Fatalf("SplitLinkText(%q, %q) produced %q", text, sep, id)
			}
		}
	})
}

// FuzzExtractCSVReferences feeds arbitrary bytes to the references.csv parser.
// On accepted input every reference must come from a profile control, with a
// trimmed, non-empty identifier and a non-empty citation.
func FuzzExtractCSVReferences(f *testing.F) {
	f.Add([]byte("source_id,target_id,citation\ns-1,AB-2,doc\ns-2,CD-1,\n"))
	f.Add([]byte("target_id,source_id\nAB-2,s-1\n"))
	f.Add([]byte("source_id,target_id\ns-9,AB-2\n"))
	f.Add([]byte("source_id,target_id\ns-1,\n"))
	f.Add([]byte("source_id,target_id\ns-1\n"))
	f.Add([]byte("source_id,target_id\n\"s-1,AB-2\n"))
	f.Add([]byte("\xff\x00,\n"))
	f.Add([]byte(""))
	src, err := crosswalk.LoadCatalog("testdata/source_a.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "r.csv"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		p := &crosswalk.Profile{Name: "fuzz", Controls: []string{"s-1", "s-2"}, References: crosswalk.ReferenceSpec{CSV: "r.csv"}}
		refs, err := crosswalk.ExtractReferences(p, dir, src)
		if err != nil {
			return
		}
		for _, r := range refs {
			if r.SourceID != "s-1" && r.SourceID != "s-2" {
				t.Fatalf("accepted source %q outside the profile controls", r.SourceID)
			}
			if r.Identifier == "" || r.Identifier != strings.TrimSpace(r.Identifier) {
				t.Fatalf("accepted identifier %q", r.Identifier)
			}
			if r.Citation == "" {
				t.Fatalf("empty citation for %+v", r)
			}
		}
	})
}
