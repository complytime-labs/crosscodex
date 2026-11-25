package crosswalk_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// FuzzMergeJudgeFile feeds arbitrary bytes to the judge file reader, which
// parses untrusted model output. On accepted input every entry must satisfy
// the documented rules and no pair may repeat.
func FuzzMergeJudgeFile(f *testing.F) {
	line := `{"source":"PO.1.1","target":"sa-8","verdict":"related","rationale":"Both define requirements."}` + "\n"
	f.Add([]byte(line))
	f.Add([]byte(line + strings.Replace(line, "related", "unrelated", 1)))
	f.Add([]byte(line + line))
	f.Add([]byte(`{"source":"a","target":"b","verdict":"disputed","rationale":"r"}` + "\n"))
	f.Add([]byte(`{"source":"a","target":"b","verdict":"related","rationale":"r","extra":1}` + "\n"))
	f.Add([]byte(`{"source":"a","target":"b","verdict":"unrelated","verdict":"related","rationale":"r"}` + "\n"))
	f.Add([]byte(`{"source":"a","target":"b","VERDICT":"related","rationale":"r"}` + "\n"))
	f.Add([]byte(`{"source":"a","target":"b","verdict":"unrelated","\u0076erdict":"related","rationale":"r"}` + "\n"))
	f.Add([]byte(`{"source":"a","target":"b","\u0056ERDICT":"related","rationale":"r"}` + "\n"))
	f.Add([]byte("\r\n\r\n" + strings.TrimSuffix(line, "\n") + "\r\n"))
	f.Add([]byte(strings.Repeat("[", 5000) + "\n"))
	f.Add([]byte(`{"source":"` + strings.Repeat("a", 129) + `","target":"b","verdict":"related","rationale":"r"}` + "\n"))
	f.Add([]byte(`{"source":"a\u0000","target":"b","verdict":"related","rationale":"\u202e"}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "judge.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := crosswalk.ReadJudgeFile(path)
		if err != nil {
			return
		}
		seen := map[crosswalk.Pair]bool{}
		for _, e := range entries {
			p := crosswalk.Pair{SourceID: e.Source, TargetID: e.Target}
			if seen[p] {
				t.Fatalf("accepted pair %s twice", p)
			}
			seen[p] = true
			if e.Source == "" || e.Target == "" || len(e.Source) > 128 || len(e.Target) > 128 {
				t.Fatalf("accepted bad IDs %+v", e)
			}
			if e.Verdict != "related" && e.Verdict != "unrelated" {
				t.Fatalf("accepted verdict %q", e.Verdict)
			}
			if strings.TrimSpace(e.Rationale) == "" || len(e.Rationale) > 4096 {
				t.Fatalf("accepted rationale of %d bytes", len(e.Rationale))
			}
		}
	})
}
