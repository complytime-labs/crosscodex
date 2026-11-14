package crosswalk

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// RawReference is a reference as the profile states it, before the identifier
// is resolved against the target catalog.
type RawReference struct {
	SourceID   string
	Identifier string
	// Citation says where the reference came from, for the report.
	Citation string
}

// Reference is a resolved (source, target) pair.
type Reference struct {
	SourceID string
	TargetID string
	Citation string
}

// References partitions raw references by resolution outcome. Unresolved and
// Ambiguous are reported, never dropped or guessed.
type References struct {
	Resolved []Reference
	// ResolvedRaw holds every raw reference that resolved, before Resolved's
	// de-duplication, so the resolution rate counts the same unit (distinct
	// source and identifier) in every state.
	ResolvedRaw []RawReference
	Unresolved  []RawReference
	Ambiguous   []RawReference
}

// SplitLinkText splits text on sep, trims each token and drops empty ones.
// sep must be non-empty.
func SplitLinkText(text, sep string) []string {
	var out []string
	for _, tok := range strings.Split(text, sep) {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// ExtractReferences returns the raw references the profile declares for its
// own controls, in profile order. root anchors the CSV path.
func ExtractReferences(p *Profile, root string, src *Catalog) ([]RawReference, error) {
	byID := map[string]oscalTypes.Control{}
	for _, c := range src.Controls() {
		if _, dup := byID[c.ID]; !dup {
			byID[c.ID] = c
		}
	}
	for _, id := range p.Controls {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("profile %q lists control %q, which is not in source catalog %q", p.Name, id, src.Path)
		}
	}
	if p.References.CSV != "" {
		return extractCSV(p, root)
	}
	return extractLinks(p, byID, src)
}

func extractLinks(p *Profile, byID map[string]oscalTypes.Control, src *Catalog) ([]RawReference, error) {
	spec := p.References.OSCALLinks
	title, err := compileSelector(spec.Resource.Title)
	if err != nil {
		return nil, fmt.Errorf("profile %q: references.oscal_links.resource.title: %w", p.Name, err)
	}
	rlink, err := compileSelector(spec.Resource.Rlink)
	if err != nil {
		return nil, fmt.Errorf("profile %q: references.oscal_links.resource.rlink: %w", p.Name, err)
	}
	resources := map[string]oscalTypes.Resource{}
	if bm := src.Doc.BackMatter; bm != nil && bm.Resources != nil {
		for _, r := range *bm.Resources {
			resources[r.UUID] = r
		}
	}

	var out []RawReference
	for _, id := range p.Controls {
		ctrl := byID[id]
		if ctrl.Links == nil {
			continue
		}
		for _, link := range *ctrl.Links {
			if link.Rel != spec.Rel || !strings.HasPrefix(link.Href, "#") {
				continue
			}
			res, ok := resources[strings.TrimPrefix(link.Href, "#")]
			if !ok {
				return nil, fmt.Errorf("control %q in %q has a %q link to %q, which is not a back-matter resource", id, src.Path, link.Rel, link.Href)
			}
			if !selects(title, rlink, res) {
				continue
			}
			cite := citation(res)
			var identifiers []string
			if spec.Identifier == identifierResourceTitle {
				if t := strings.TrimSpace(res.Title); t != "" {
					identifiers = []string{t}
				}
			} else {
				identifiers = SplitLinkText(link.Text, spec.Separator)
			}
			for _, ident := range identifiers {
				out = append(out, RawReference{SourceID: id, Identifier: ident, Citation: cite})
			}
		}
	}
	return out, nil
}

// selects reports whether res satisfies every selector that is set.
func selects(title, rlink *regexp.Regexp, res oscalTypes.Resource) bool {
	if title != nil && !title.MatchString(res.Title) {
		return false
	}
	if rlink != nil {
		if res.Rlinks == nil || !slices.ContainsFunc(*res.Rlinks, func(l oscalTypes.ResourceLink) bool { return rlink.MatchString(l.Href) }) {
			return false
		}
	}
	return true
}

// citation is the resource title plus its first rlink href, when there is one.
func citation(res oscalTypes.Resource) string {
	if res.Rlinks != nil && len(*res.Rlinks) > 0 {
		return strings.TrimSpace(res.Title + " " + (*res.Rlinks)[0].Href)
	}
	return res.Title
}

// readCSV returns the bytes of the profile's references.csv under root.
func readCSV(p *Profile, root string) ([]byte, error) {
	rel := p.References.CSV
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("profile %q: references.csv %q must be a path inside the repository root", p.Name, rel)
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return nil, fmt.Errorf("opening references.csv %q: %w", rel, err)
	}
	return data, nil
}

// CSVDigest returns the hex SHA-256 of the profile's references.csv, or "" when
// the profile does not use one, so a report names the exact yardstick bytes.
func CSVDigest(p *Profile, root string) (string, error) {
	if p.References.CSV == "" {
		return "", nil
	}
	data, err := readCSV(p, root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func extractCSV(p *Profile, root string) ([]RawReference, error) {
	rel := p.References.CSV
	data, err := readCSV(p, root)
	if err != nil {
		return nil, err
	}

	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s has no header: expected source_id,target_id[,citation]", rel)
	}
	if err != nil {
		return nil, fmt.Errorf("%s line 1: %w", rel, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	srcCol, okS := col["source_id"]
	tgtCol, okT := col["target_id"]
	if !okS || !okT {
		return nil, fmt.Errorf("%s header must contain source_id and target_id columns (found %q)", rel, header)
	}
	citeCol, hasCite := col["citation"]
	inScope := map[string]bool{}
	for _, id := range p.Controls {
		inScope[id] = true
	}

	var out []RawReference
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			// FieldPos panics when Read failed before recording a field, so the
			// line comes from the parse error itself.
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				return nil, fmt.Errorf("%s line %d: %w", rel, pe.Line, err)
			}
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		line, _ := r.FieldPos(0)
		if len(rec) <= max(srcCol, tgtCol) {
			return nil, fmt.Errorf("%s line %d: row has %d fields, fewer than the header's source_id and target_id columns", rel, line, len(rec))
		}
		source, target := strings.TrimSpace(rec[srcCol]), strings.TrimSpace(rec[tgtCol])
		if !inScope[source] {
			return nil, fmt.Errorf("%s line %d: source_id %q is not one of profile %q's controls", rel, line, source, p.Name)
		}
		if target == "" {
			return nil, fmt.Errorf("%s line %d: target_id is empty", rel, line)
		}
		cite := "csv:" + rel
		if hasCite && citeCol < len(rec) && strings.TrimSpace(rec[citeCol]) != "" {
			cite = strings.TrimSpace(rec[citeCol])
		}
		out = append(out, RawReference{SourceID: source, Identifier: target, Citation: cite})
	}
}

// ResolveReferences resolves each raw identifier against idx. Resolved pairs
// are de-duplicated by (SourceID, TargetID), keeping the first citation.
func ResolveReferences(raw []RawReference, idx *TargetIndex) References {
	var out References
	seen := map[[2]string]bool{}
	for _, ref := range raw {
		res := idx.Resolve(ref.Identifier)
		switch res.State {
		case Resolved:
			out.ResolvedRaw = append(out.ResolvedRaw, ref)
			key := [2]string{ref.SourceID, res.ControlID}
			if !seen[key] {
				seen[key] = true
				out.Resolved = append(out.Resolved, Reference{SourceID: ref.SourceID, TargetID: res.ControlID, Citation: ref.Citation})
			}
		case Ambiguous:
			out.Ambiguous = append(out.Ambiguous, ref)
		default:
			out.Unresolved = append(out.Unresolved, ref)
		}
	}
	return out
}
