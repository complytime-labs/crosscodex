package crosswalk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Verdicts a label can carry.
const (
	VerdictRelated   = "related"
	VerdictUnrelated = "unrelated"
	VerdictDisputed  = "disputed"
)

const (
	judgeHuman = "human"
	// labelsVersion is the only label file format version.
	labelsVersion = "1"
	// maxLabelsBytes bounds the label file read: a seeded file holds a few
	// hundred entries of a few hundred bytes each.
	maxLabelsBytes = 8 << 20
	// maxRationaleBytes bounds a rationale; the file is untrusted input and
	// rationales are kept only in the file, not rendered in reports.
	maxRationaleBytes = 4096
	// maxIDBytes bounds a source or target control ID. Real IDs are short
	// (SP 800-53 enhancement parts such as "sa-15.7_odp.01" are under 20
	// bytes; UUIDs are 36), so 128 leaves room without admitting blobs.
	maxIDBytes      = 128
	labelDateLayout = "2006-01-02"
)

var (
	labelVerdicts = []string{VerdictRelated, VerdictUnrelated, VerdictDisputed}
	labelJudges   = []string{"opus", "sonnet", judgeHuman}
	labelFields   = []string{"source", "target", "verdict", "judged_by", "rationale", "date"}
)

// Label is one judgement of a candidate pair. Source and Target are control
// IDs exactly as the run exports them. Line is the entry's line in the label
// file.
type Label struct {
	Source, Target, Verdict string
	JudgedBy                []string
	Rationale               string
	// Date is YYYY-MM-DD.
	Date string
	Line int
}

func (l Label) byHuman() bool { return slices.Contains(l.JudgedBy, judgeHuman) }

// LabelsDigest returns the hex SHA-256 of the labels file's bytes, so a report
// names the exact labels it was scored with.
func LabelsDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading labels %q: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// LoadLabels reads a label file and returns its judgements by pair. The file
// is untrusted input: unknown keys, aliases, missing or malformed fields and
// repeated pairs are errors that name the file, line and rule. A pair judged
// by a human takes precedence over model judgements of it, whatever their
// order; two judgements of one pair by humans, or by models only, are
// rejected.
func LoadLabels(path string) (map[Pair]Label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading labels %q: %w", path, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLabelsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading labels %q: %w", path, err)
	}
	if len(raw) > maxLabelsBytes {
		return nil, fmt.Errorf("labels %q: file exceeds %d bytes: split it or remove entries for pairs no run produces", path, maxLabelsBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("labels %q is empty: expected 'version: 1' and a 'pairs' list", path)
		}
		return nil, fmt.Errorf("decoding labels %q: %w", path, err)
	}
	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("labels %q is empty: expected 'version: 1' and a 'pairs' list", path)
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("labels %q must be a single YAML document", path)
	}
	if err := rejectAliases(&doc); err != nil {
		return nil, fmt.Errorf("labels %q line %d: %w", path, err.line, err.err)
	}
	labels, lerr := parseLabels(doc.Content[0])
	if lerr != nil {
		return nil, fmt.Errorf("labels %q line %d: %w", path, lerr.line, lerr.err)
	}
	return labels, nil
}

// lineError is a rule violation at a YAML line.
type lineError struct {
	line int
	err  error
}

func at(n *yaml.Node, format string, args ...any) *lineError {
	return &lineError{line: n.Line, err: fmt.Errorf(format, args...)}
}

// rejectAliases refuses anchors' aliases anywhere: labels are flat data, and
// aliases are how a small file expands into a huge one.
func rejectAliases(n *yaml.Node) *lineError {
	if n.Kind == yaml.AliasNode {
		return at(n, "rule alias: YAML aliases are not allowed in a labels file; write the value out")
	}
	for _, c := range n.Content {
		if err := rejectAliases(c); err != nil {
			return err
		}
	}
	return nil
}

// mappingNode returns n's key/value nodes by key, rejecting a non-mapping, an
// unknown or repeated key, and a non-scalar key.
func mappingNode(n *yaml.Node, what string, allowed []string) (map[string]*yaml.Node, *lineError) {
	if n.Kind != yaml.MappingNode {
		return nil, at(n, "rule %s: must be a mapping with keys %s", what, strings.Join(allowed, ", "))
	}
	out := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || !slices.Contains(allowed, k.Value) {
			return nil, at(k, "rule unknown-field: %s has no field %q; the fields are %s", what, k.Value, strings.Join(allowed, ", "))
		}
		if _, dup := out[k.Value]; dup {
			return nil, at(k, "rule duplicate-field: %s repeats field %q", what, k.Value)
		}
		out[k.Value] = v
	}
	return out, nil
}

func parseLabels(root *yaml.Node) (map[Pair]Label, *lineError) {
	top, err := mappingNode(root, "the labels file", []string{"version", "pairs"})
	if err != nil {
		return nil, err
	}
	v, ok := top["version"]
	if !ok {
		return nil, at(root, "rule version: 'version: %s' is required", labelsVersion)
	}
	if v.Kind != yaml.ScalarNode || v.Value != labelsVersion {
		return nil, at(v, "rule version: version must be %s, got %q", labelsVersion, v.Value)
	}
	labels := map[Pair]Label{}
	humanSeen, modelSeen := map[Pair]Label{}, map[Pair]Label{}
	pairs, ok := top["pairs"]
	if !ok || pairs.Tag == "!!null" {
		return labels, nil
	}
	if pairs.Kind != yaml.SequenceNode {
		return nil, at(pairs, "rule pairs: pairs must be a list of entries")
	}
	for _, e := range pairs.Content {
		l, err := parseLabel(e)
		if err != nil {
			return nil, err
		}
		key := Pair{l.Source, l.Target}
		// Humans and models are tracked apart, so a human entry between two
		// model entries cannot hide the second from the duplicate check.
		seen := modelSeen
		if l.byHuman() {
			seen = humanSeen
		}
		if prev, dup := seen[key]; dup {
			if l.byHuman() {
				return nil, at(e, "rule duplicate-pair: %s is judged by a human on lines %d and %d: keep one entry", key, prev.Line, l.Line)
			}
			return nil, at(e, "rule duplicate-pair: %s is judged on lines %d and %d: keep one entry, or add a human entry to override", key, prev.Line, l.Line)
		}
		seen[key] = l
		if _, human := humanSeen[key]; !human || l.byHuman() {
			labels[key] = l
		}
	}
	return labels, nil
}

func parseLabel(e *yaml.Node) (Label, *lineError) {
	f, err := mappingNode(e, "a pairs entry", labelFields)
	if err != nil {
		return Label{}, err
	}
	l := Label{Line: e.Line}
	str := func(field string, dst *string, maxBytes int) *lineError {
		n, ok := f[field]
		if !ok {
			return at(e, "rule %s: required field %q is missing", field, field)
		}
		if n.Kind != yaml.ScalarNode || strings.TrimSpace(n.Value) == "" {
			return at(n, "rule %s: %s must be a non-blank scalar", field, field)
		}
		if strings.IndexFunc(n.Value, unicode.IsControl) >= 0 {
			return at(n, "rule %s: %s must not contain control characters", field, field)
		}
		if len(n.Value) > maxBytes {
			return at(n, "rule %s: %s is %d bytes; the limit is %d", field, field, len(n.Value), maxBytes)
		}
		*dst = n.Value
		return nil
	}
	for field, dst := range map[string]*string{"source": &l.Source, "target": &l.Target, "verdict": &l.Verdict, "date": &l.Date} {
		if err := str(field, dst, maxIDBytes); err != nil {
			return Label{}, err
		}
	}
	if !slices.Contains(labelVerdicts, l.Verdict) {
		return Label{}, at(f["verdict"], "rule verdict: verdict %q must be one of %s", l.Verdict, strings.Join(labelVerdicts, ", "))
	}
	if t, err := time.Parse(labelDateLayout, l.Date); err != nil || t.Format(labelDateLayout) != l.Date {
		return Label{}, at(f["date"], "rule date: date %q must be a real calendar date as YYYY-MM-DD", l.Date)
	}
	rn, ok := f["rationale"]
	if !ok {
		return Label{}, at(e, "rule rationale: required field \"rationale\" is missing")
	}
	if rn.Kind != yaml.ScalarNode || strings.TrimSpace(rn.Value) == "" {
		return Label{}, at(rn, "rule rationale: rationale must be a non-blank scalar")
	}
	if len(rn.Value) > maxRationaleBytes {
		return Label{}, at(rn, "rule rationale: rationale is %d bytes; the limit is %d", len(rn.Value), maxRationaleBytes)
	}
	l.Rationale = rn.Value

	jn, ok := f["judged_by"]
	if !ok {
		return Label{}, at(e, "rule judged_by: required field \"judged_by\" is missing")
	}
	if jn.Kind != yaml.SequenceNode || len(jn.Content) == 0 {
		return Label{}, at(jn, "rule judged_by: judged_by must be a non-empty list drawn from %s", strings.Join(labelJudges, ", "))
	}
	for _, j := range jn.Content {
		switch {
		case j.Kind != yaml.ScalarNode || !slices.Contains(labelJudges, j.Value):
			return Label{}, at(j, "rule judged_by: judge %q must be one of %s", j.Value, strings.Join(labelJudges, ", "))
		case slices.Contains(l.JudgedBy, j.Value):
			return Label{}, at(j, "rule judged_by: judge %q is listed twice", j.Value)
		}
		l.JudgedBy = append(l.JudgedBy, j.Value)
	}
	return l, nil
}
