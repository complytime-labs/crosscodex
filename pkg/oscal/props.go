package oscal

import (
	"strings"
	"unicode"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// Published-prop vocabulary. A catalog publishes how it writes an item's name
// ("AC-2(7)", "d.") in a `label` prop and its ordering key in `sort-id`; both
// are stored verbatim in ControlItem.Props under these keys.
const (
	PropLabel  = "label"
	PropSortID = "sort-id"
)

// nistNamespace is the OSCAL core namespace; a prop without an ns attribute
// lives in it implicitly.
const nistNamespace = "http://csrc.nist.gov/ns/oscal"

// selectProp returns the value of the prop called name that a catalog uses to
// publish an item's label or sort-id. Only props in the OSCAL namespace (no ns
// or nistNamespace) with a non-blank value count. Among them the first without
// a class wins, since a class marks a variant (zero-padded, sp800-53a); when
// every candidate has a class the first in document order wins. The value is
// returned verbatim; ok is false when no prop qualifies.
func selectProp(props *[]oscalTypes.Property, name string) (value string, ok bool) {
	if props == nil {
		return "", false
	}
	var first *oscalTypes.Property
	for i := range *props {
		p := &(*props)[i]
		if p.Name != name || (p.Ns != "" && p.Ns != nistNamespace) || strings.TrimSpace(p.Value) == "" {
			continue
		}
		if p.Class == "" {
			return p.Value, true
		}
		if first == nil {
			first = p
		}
	}
	if first == nil {
		return "", false
	}
	return first.Value, true
}

// setPublishedProps copies the selected label and sort-id of props into dst,
// leaving a key absent when the catalog publishes none.
func setPublishedProps(dst map[string]string, props *[]oscalTypes.Property) {
	if v, ok := selectProp(props, PropLabel); ok {
		dst[PropLabel] = v
	}
	if v, ok := selectProp(props, PropSortID); ok {
		dst[PropSortID] = v
	}
}

// UsableTitle returns the trimmed title, or "" when the title carries no text
// of its own. A title is unusable when
//   - it has no letter or digit (blank or punctuation only);
//   - trimmed and case-folded, it equals the identifier or the label; or
//   - it contains a digit and its key (case-folded, keeping only Unicode
//     letters and digits) equals the key of the identifier or of the label.
//
// Some catalogs title a control with its own number in another spelling
// ("MANAGE 4.2" for MANAGE_4.2, "AC-2" for ac2); such a title must not be
// shown as a heading or mistaken for requirement text. The key match needs a
// digit because a slug identifier can spell a real title: "Enable Authselect"
// for enable_authselect is the requirement, not a number.
func UsableTitle(title, identifier, label string) string {
	trimmed := strings.TrimSpace(title)
	key := titleKey(trimmed)
	if key == "" {
		return ""
	}
	for _, name := range []string{identifier, label} {
		if strings.EqualFold(trimmed, strings.TrimSpace(name)) {
			return ""
		}
		if key == titleKey(name) && strings.IndexFunc(trimmed, unicode.IsDigit) >= 0 {
			return ""
		}
	}
	return trimmed
}

// titleKey case-folds s and drops every rune that is not a letter or digit.
// Each rune folds to the smallest rune of its case-folding orbit, so keys are
// equal exactly when strings.EqualFold would match them rune by rune.
func titleKey(s string) string {
	return strings.Map(func(r rune) rune {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return -1
		}
		folded := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			folded = min(folded, f)
		}
		return folded
	}, s)
}
