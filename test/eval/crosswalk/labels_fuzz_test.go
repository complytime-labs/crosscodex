package crosswalk_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// FuzzLoadLabels feeds arbitrary bytes to the strict labels loader. On
// accepted input every label must satisfy the documented field rules, and
// the digest must hash the bytes parsed.
func FuzzLoadLabels(f *testing.F) {
	entry := "  - source: PO.1.1\n    target: sa-8\n    verdict: related\n    judged_by: [opus, sonnet]\n    rationale: Both define requirements.\n    date: 2026-11-12\n"
	f.Add([]byte("version: 1\npairs:\n" + entry))
	f.Add([]byte("\xef\xbb\xbfversion: 1\r\npairs:\r\n" + strings.ReplaceAll(entry, "\n", "\r\n")))
	f.Add([]byte("version: 1\npairs:\n" + entry + entry))
	f.Add([]byte("version: 1\npairs:\n" + strings.Replace(entry, "2026-11-12", "2026-02-30", 1)))
	f.Add([]byte("version: 1\npairs:\n" + strings.Replace(entry, "related", "maybe", 1)))
	f.Add([]byte("version: 1\npairs: &a [*a]\n"))
	f.Add([]byte(strings.Repeat("[", 5000)))
	f.Add([]byte("version: 1\nsurprise: true\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "labels.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		labels, digest, err := crosswalk.LoadLabels(path)
		if err != nil {
			return
		}
		if sum := sha256.Sum256(data); digest != hex.EncodeToString(sum[:]) {
			t.Fatalf("digest %s does not hash the bytes parsed", digest)
		}
		for pair, l := range labels {
			if pair.SourceID != l.Source || pair.TargetID != l.Target || l.Source == "" || l.Target == "" {
				t.Fatalf("pair %v does not match label %+v", pair, l)
			}
			if l.Verdict != "related" && l.Verdict != "unrelated" && l.Verdict != "disputed" {
				t.Fatalf("accepted verdict %q", l.Verdict)
			}
			if len(l.JudgedBy) == 0 || l.Line < 1 || l.Rationale == "" {
				t.Fatalf("accepted incomplete label %+v", l)
			}
			if _, err := time.Parse("2006-01-02", l.Date); err != nil {
				t.Fatalf("accepted date %q: %v", l.Date, err)
			}
		}
	})
}
