package crosswalk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	// maxPairsBytes bounds a label-input file: each line carries both
	// controls' composed text, so it is far larger than a labels file.
	maxPairsBytes = 128 << 20
	// maxPairsLineBytes bounds one label-input line (two control statements).
	maxPairsLineBytes = 1 << 20
	// maxPairsLines bounds the pairs a merge requires.
	maxPairsLines = 200_000
	// maxJudgeBytes bounds a judge file, like the labels file it feeds.
	maxJudgeBytes = maxLabelsBytes
	// maxJudgeLineBytes bounds one judge line: a rationale, two IDs and
	// JSON escaping (up to 6 bytes per control character) fit well inside.
	maxJudgeLineBytes = 32 << 10
	// maxJudgeLines bounds the entries of a judge file, as many as a labels
	// file of maxLabelsBytes holds.
	maxJudgeLines = 50_000
	// maxJudgeRationaleBytes bounds a judge's rationale so that both judges'
	// rationales, labelled, fit in one label's maxRationaleBytes when the
	// judges disagree. The 64 bytes cover "opus: ", "sonnet: " and the
	// separator.
	maxJudgeRationaleBytes = (maxRationaleBytes - 64) / 2

	labelsFileMode = 0o644
)

// JudgeEntry is one line of a judge file: a model's verdict on one pair.
type JudgeEntry struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Verdict   string `json:"verdict"`
	Rationale string `json:"rationale"`
}

// MergeRequest names the files of a labels merge.
type MergeRequest struct {
	// Existing is the labels file to merge into; empty starts from no labels.
	Existing string
	// Pairs is a label-input JSONL file: it defines the required pairs.
	Pairs string
	// Judges maps a model judge name (opus, sonnet) to its judge file.
	Judges map[string]string
	// Out is the labels file to write.
	Out string
	// Date is the YYYY-MM-DD stamped on new entries.
	Date string
}

// modelJudges returns the loader's judges that are models, in loader order.
func modelJudges() []string {
	return slices.DeleteFunc(slices.Clone(labelJudges), func(j string) bool { return j == judgeHuman })
}

// readBounded reads path, refusing a file over max bytes.
func readBounded(path, what string, limit int) ([]byte, error) {
	raw, err := readCapped(path, what, int64(limit))
	if errors.Is(err, errOverCap) {
		return nil, fmt.Errorf("%s %q exceeds %d bytes: check that it is the file the merge expects", what, path, limit)
	}
	return raw, err
}

// eachLine calls fn with every non-blank line of raw and its 1-based number.
// A line over maxLine bytes, or more than maxLines non-blank lines, is an
// error naming the file. Iterating by index keeps a file of newlines from
// allocating a slice per line.
func eachLine(raw []byte, name string, maxLine, maxLines int, fn func(line []byte) error) error {
	n, count := 0, 0
	for len(raw) > 0 {
		n++
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		if len(line) > maxLine {
			return fmt.Errorf("%s line %d: line is longer than %d bytes", name, n, maxLine)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		count++
		if count > maxLines {
			return fmt.Errorf("%s line %d: more than %d entries", name, n, maxLines)
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("%s line %d: %w", name, n, err)
		}
	}
	return nil
}

// checkID applies the labels loader's rules for a control ID.
func checkID(field, v string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("%s must be a non-blank string", field)
	case strings.IndexFunc(v, unicode.IsControl) >= 0:
		return fmt.Errorf("%s must not contain control characters", field)
	case len(v) > maxIDBytes:
		return fmt.Errorf("%s is %d bytes; the limit is %d", field, len(v), maxIDBytes)
	}
	return nil
}

// ReadPairs reads a label-input JSONL file and returns its pairs in file
// order. Only the source and target fields are used; the others (titles and
// text) are ignored. A repeated pair, or a file with no pairs, is an error.
func ReadPairs(path string) ([]Pair, error) {
	raw, err := readBounded(path, "pairs file", maxPairsBytes)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("pairs file %q", path)
	var pairs []Pair
	lines := map[Pair]int{}
	lineNo := 0
	err = eachLine(raw, name, maxPairsLineBytes, maxPairsLines, func(line []byte) error {
		lineNo++
		var row struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("not a label-input JSON object: %w", err)
		}
		for field, v := range map[string]string{"source": row.Source, "target": row.Target} {
			if err := checkID(field, v); err != nil {
				return err
			}
		}
		p := Pair{SourceID: row.Source, TargetID: row.Target}
		if _, dup := lines[p]; dup {
			return fmt.Errorf("pair %s repeats an earlier entry", p)
		}
		lines[p] = lineNo
		pairs = append(pairs, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("pairs file %q has no pairs: pass the output of crosswalk-eval label-input", path)
	}
	return pairs, nil
}

// ReadJudgeFile reads a judge file: JSONL, one {"source","target","verdict",
// "rationale"} object per line. It is untrusted model output, so decoding is
// strict (unknown fields, trailing data, a verdict other than related or
// unrelated, blank or over-long fields and a repeated pair are errors that
// name the file and line) and the file, line length and entry count are
// bounded.
func ReadJudgeFile(path string) ([]JudgeEntry, error) {
	raw, err := readBounded(path, "judge file", maxJudgeBytes)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("judge file %q", path)
	var entries []JudgeEntry
	first := map[Pair]int{}
	lineNo := 0
	err = eachLine(raw, name, maxJudgeLineBytes, maxJudgeLines, func(line []byte) error {
		lineNo++
		if err := checkJudgeKeys(line); err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var e JudgeEntry
		if err := dec.Decode(&e); err != nil {
			return fmt.Errorf("not a judge entry {source, target, verdict, rationale}: %w", err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return errors.New("a line must hold a single JSON object")
		}
		if err := validJudgeEntry(e); err != nil {
			return err
		}
		p := Pair{SourceID: e.Source, TargetID: e.Target}
		if prev, dup := first[p]; dup {
			return fmt.Errorf("pair %s is already judged on line %d: keep one entry per pair", p, prev)
		}
		first[p] = lineNo
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// judgeKeys are the only keys a judge line may hold.
var judgeKeys = map[string]bool{"source": true, "target": true, "verdict": true, "rationale": true}

// checkJudgeKeys scans one judge line's top-level object token by token and
// refuses what encoding/json would quietly accept: a repeated key (the last
// would win), a key that is not exactly one of judgeKeys (the decoder matches
// keys case-insensitively) and a value that is not a string. A line that is
// not a JSON object is left to the strict decode that follows.
func checkJudgeKeys(line []byte) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil
		}
		if !judgeKeys[key] {
			return fmt.Errorf("key %q must be exactly one of source, target, verdict, rationale (keys are case-sensitive)", key)
		}
		if seen[key] {
			return fmt.Errorf("key %q appears more than once: keep one value per key", key)
		}
		seen[key] = true
		val, err := dec.Token()
		if err != nil {
			return nil
		}
		if _, isString := val.(string); !isString {
			return fmt.Errorf("key %q must hold a string", key)
		}
	}
	return nil
}

func validJudgeEntry(e JudgeEntry) error {
	for field, v := range map[string]string{"source": e.Source, "target": e.Target} {
		if err := checkID(field, v); err != nil {
			return err
		}
	}
	if e.Verdict != VerdictRelated && e.Verdict != VerdictUnrelated {
		return fmt.Errorf("verdict %q must be %s or %s: a judge does not emit %s", e.Verdict, VerdictRelated, VerdictUnrelated, VerdictDisputed)
	}
	if strings.TrimSpace(e.Rationale) == "" {
		return errors.New("rationale must be non-blank")
	}
	if len(e.Rationale) > maxJudgeRationaleBytes {
		return fmt.Errorf("rationale is %d bytes; the limit is %d", len(e.Rationale), maxJudgeRationaleBytes)
	}
	return nil
}

// sortedPairs returns the keys of m ordered by source then target.
func sortedPairs[V any](m map[Pair]V) []Pair {
	out := make([]Pair, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Pair) int {
		return cmpStrings(a.SourceID, b.SourceID, a.TargetID, b.TargetID)
	})
	return out
}

func cmpStrings(a1, a2, b1, b2 string) int {
	if c := strings.Compare(a1, a2); c != 0 {
		return c
	}
	return strings.Compare(b1, b2)
}

// MergeLabels merges the judges' verdicts on pairs into existing labels and
// returns the result ordered by source then target, and the pairs the judges
// disagreed on that no human judged. Every judge must cover every pair and
// judge no other: otherwise the error lists all gaps and nothing is merged.
// Agreement yields one entry with the judges (in loader order) and the first
// judge's rationale; disagreement yields a disputed entry holding every
// judge's labelled rationale. An existing human entry is kept as is and
// suppresses the model entry for its pair; other existing entries for pairs in
// pairs are replaced and those for other pairs are kept. date stamps new
// entries.
func MergeLabels(existing []Label, judges map[string][]JudgeEntry, pairs []Pair, date string) ([]Label, []Pair, error) {
	if t, err := time.Parse(labelDateLayout, date); err != nil || t.Format(labelDateLayout) != date {
		return nil, nil, fmt.Errorf("date %q must be a real calendar date as YYYY-MM-DD", date)
	}
	if len(judges) == 0 {
		return nil, nil, errors.New("a merge needs at least one judge")
	}
	names := modelJudges()
	for name := range judges {
		if !slices.Contains(names, name) {
			return nil, nil, fmt.Errorf("judge %q must be one of %s: human entries are kept from the existing labels file", name, strings.Join(names, ", "))
		}
	}
	required := map[Pair]bool{}
	for _, p := range pairs {
		required[p] = true
	}
	byPair := map[Pair]Label{}
	for _, l := range existing {
		p := Pair{SourceID: l.Source, TargetID: l.Target}
		if _, dup := byPair[p]; dup {
			return nil, nil, fmt.Errorf("existing labels judge pair %s twice", p)
		}
		byPair[p] = l
	}

	// Index each judge's entries and collect every coverage gap before failing.
	verdicts := map[string]map[Pair]JudgeEntry{}
	var problems []string
	for _, name := range names {
		entries, ok := judges[name]
		if !ok {
			continue
		}
		idx := map[Pair]JudgeEntry{}
		for _, e := range entries {
			p := Pair{SourceID: e.Source, TargetID: e.Target}
			if _, dup := idx[p]; dup {
				return nil, nil, fmt.Errorf("judge %s judges pair %s twice", name, p)
			}
			idx[p] = e
		}
		verdicts[name] = idx
		var missing, extra []string
		for _, p := range pairs {
			if _, ok := idx[p]; !ok {
				missing = append(missing, p.String())
			}
		}
		for _, p := range sortedPairs(idx) {
			if !required[p] {
				extra = append(extra, p.String())
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("judge %s is missing %d pairs: %s", name, len(missing), strings.Join(missing, "; ")))
		}
		if len(extra) > 0 {
			problems = append(problems, fmt.Sprintf("judge %s judges %d pairs not in --pairs: %s", name, len(extra), strings.Join(extra, "; ")))
		}
	}
	if len(problems) > 0 {
		return nil, nil, fmt.Errorf("judges do not match the pairs file, nothing merged: %s", strings.Join(problems, ". "))
	}

	var disputed []Pair
	var judged []string
	for _, name := range names {
		if _, ok := verdicts[name]; ok {
			judged = append(judged, name)
		}
	}
	for _, p := range pairs {
		if old, ok := byPair[p]; ok && old.byHuman() {
			continue
		}
		first := verdicts[judged[0]][p]
		l := Label{Source: p.SourceID, Target: p.TargetID, Verdict: first.Verdict, JudgedBy: slices.Clone(judged), Rationale: first.Rationale, Date: date}
		agree := true
		for _, name := range judged[1:] {
			agree = agree && verdicts[name][p].Verdict == first.Verdict
		}
		if !agree {
			var parts []string
			for _, name := range judged {
				parts = append(parts, name+": "+verdicts[name][p].Rationale)
			}
			l.Verdict, l.Rationale = VerdictDisputed, strings.Join(parts, "\n")
			disputed = append(disputed, p)
		}
		byPair[p] = l
	}
	merged := make([]Label, 0, len(byPair))
	for _, p := range sortedPairs(byPair) {
		merged = append(merged, byPair[p])
	}
	slices.SortFunc(disputed, func(a, b Pair) int { return cmpStrings(a.SourceID, b.SourceID, a.TargetID, b.TargetID) })
	return merged, disputed, nil
}

// labelsDoc and labelEntry are the labels file as written; field order is the
// file's key order.
type labelsDoc struct {
	Version yaml.Node    `yaml:"version"`
	Pairs   []labelEntry `yaml:"pairs"`
}

type labelEntry struct {
	Source    string   `yaml:"source"`
	Target    string   `yaml:"target"`
	Verdict   string   `yaml:"verdict"`
	JudgedBy  []string `yaml:"judged_by,flow"`
	Rationale string   `yaml:"rationale"`
	Date      string   `yaml:"date"`
}

// RenderLabels renders labels as a labels file, in the order given. The
// output is deterministic. It is not checked against the loader; MergeLabelFiles
// does that before writing.
func RenderLabels(labels []Label) ([]byte, error) {
	doc := labelsDoc{Version: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: labelsVersion}}
	for _, l := range labels {
		doc.Pairs = append(doc.Pairs, labelEntry{l.Source, l.Target, l.Verdict, l.JudgedBy, l.Rationale, l.Date})
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering labels: %w", err)
	}
	return raw, nil
}

// MergeLabelFiles merges the judge files into the existing labels (if any) for
// the pairs file's pairs and writes the result to req.Out, returning the pairs
// the judges disputed that no human judged. Nothing is written unless the
// whole merge succeeds and the rendered file passes the strict labels loader;
// the write is a temp file in Out's directory renamed over Out, so Out is
// never partial. A symlink at Out is replaced, not written through.
func MergeLabelFiles(req MergeRequest) ([]Pair, error) {
	var existing []Label
	if req.Existing != "" {
		loaded, _, err := LoadLabels(req.Existing)
		if err != nil {
			return nil, err
		}
		for _, p := range sortedPairs(loaded) {
			existing = append(existing, loaded[p])
		}
	}
	pairs, err := ReadPairs(req.Pairs)
	if err != nil {
		return nil, err
	}
	judges := map[string][]JudgeEntry{}
	for name, path := range req.Judges {
		if !slices.Contains(modelJudges(), name) {
			return nil, fmt.Errorf("judge %q must be one of %s", name, strings.Join(modelJudges(), ", "))
		}
		if judges[name], err = ReadJudgeFile(path); err != nil {
			return nil, err
		}
	}
	merged, disputed, err := MergeLabels(existing, judges, pairs, req.Date)
	if err != nil {
		return nil, err
	}
	raw, err := RenderLabels(merged)
	if err != nil {
		return nil, err
	}
	back, _, err := decodeLabels(req.Out, raw)
	if err != nil {
		return nil, fmt.Errorf("merged labels fail the strict loader, not written: %w", err)
	}
	if err := checkRoundTrip(merged, back); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(req.Out, raw); err != nil {
		return nil, err
	}
	return disputed, nil
}

// checkRoundTrip refuses a merge whose rendered file does not read back as the
// merged labels: same pairs, and for each pair the same verdict, judges,
// rationale and date. Line numbers are positions in the file and not compared.
func checkRoundTrip(merged []Label, back map[Pair]Label) error {
	if len(back) != len(merged) {
		return fmt.Errorf("merged labels hold %d entries but %d read back, not written: this is a bug in the label renderer, report it", len(merged), len(back))
	}
	for _, want := range merged {
		p := Pair{SourceID: want.Source, TargetID: want.Target}
		got, ok := back[p]
		if !ok || got.Verdict != want.Verdict || !slices.Equal(got.JudgedBy, want.JudgedBy) || got.Rationale != want.Rationale || got.Date != want.Date {
			return fmt.Errorf("merged label %s differs after rendering and reading back, not written: this is a bug in the label renderer, report it", p)
		}
	}
	return nil
}

// writeFileAtomic writes data to path through a temp file in path's directory
// and a rename, so path holds either its old content or all of data.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".labels-*.tmp")
	if err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := tmp.Chmod(labelsFileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	return nil
}
