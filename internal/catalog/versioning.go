package catalog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidDocumentSlug and ErrInvalidVersionLabel are wrapped by the
// validators below. Callers classify with errors.Is, never by message text.
var (
	ErrInvalidDocumentSlug = errors.New("invalid document slug")
	ErrInvalidVersionLabel = errors.New("invalid version label")
)

const (
	maxVersionLabelChars = 64
	// hashPrefixLen is the number of leading hex characters of a content hash
	// shown to humans.
	hashPrefixLen = 6
	// unstableIDShareDivisor: ids are "unstable" when shared ids are fewer than
	// 1/10 (10%) of the larger version's id count.
	unstableIDShareDivisor = 10
)

var documentSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateDocumentSlug checks that s is a usable catalog document slug.
func ValidateDocumentSlug(s string) error {
	if !documentSlugPattern.MatchString(s) {
		return fmt.Errorf("%w %q: a slug is 1-63 characters of a-z0-9 and '-', starting with a letter or digit; choose a slug such as \"nist-800-53\"",
			ErrInvalidDocumentSlug, s)
	}
	return nil
}

// ValidateVersionLabel checks that s is 1-64 valid UTF-8 characters with no
// control characters (including newlines), no Unicode format characters (such
// as bidi overrides and zero-width characters), no line or paragraph
// separators and no '<', '>' or '`' (markup), so it is safe to print and to
// place in prompts and logs.
func ValidateVersionLabel(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: label is not valid UTF-8; supply a UTF-8 label", ErrInvalidVersionLabel)
	}
	if n := utf8.RuneCountInString(s); n < 1 || n > maxVersionLabelChars {
		return fmt.Errorf("%w: label must be 1-64 characters, got %d; shorten it or omit the label to use the OSCAL version or import date",
			ErrInvalidVersionLabel, n)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: label contains a control character (U+%04X); remove newlines, tabs and other control characters",
				ErrInvalidVersionLabel, r)
		}
		if unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("%w: label contains a format character (U+%04X); remove invisible, direction-override and line-separator characters",
				ErrInvalidVersionLabel, r)
		}
	}
	if i := strings.IndexAny(s, "<>`"); i >= 0 {
		return fmt.Errorf("%w: label contains markup character %q; remove '<', '>' and '`'", ErrInvalidVersionLabel, s[i])
	}
	return nil
}

func dateAndHash(importedAt time.Time, contentHash string) string {
	prefix := contentHash
	if len(prefix) > hashPrefixLen {
		prefix = prefix[:hashPrefixLen]
	}
	return importedAt.UTC().Format("2006-01-02") + " · " + prefix
}

// ResolveVersionLabel picks the label for a catalog version: the explicit
// label, else the OSCAL metadata.version, else "<UTC date> · <hash prefix>".
func ResolveVersionLabel(explicit, oscalVersion string, importedAt time.Time, contentHash string) string {
	if explicit != "" {
		return explicit
	}
	if oscalVersion != "" {
		return oscalVersion
	}
	return dateAndHash(importedAt, contentHash)
}

// FormatVersion renders a version for humans as "slug @ label (date · hash6)".
func FormatVersion(slug, label string, importedAt time.Time, contentHash string) string {
	return fmt.Sprintf("%s @ %s (%s)", slug, label, dateAndHash(importedAt, contentHash))
}

// ChangeReport lists control identifiers that differ between two versions.
// IDsUnstable is true when the versions share so few identifiers that the
// per-id comparison is unreliable (e.g. the publisher renumbered everything);
// the lists are then still accurate but should not be read as an edit history.
type ChangeReport struct {
	Added, Removed, Changed []string
	IDsUnstable             bool
}

// DiffControls compares two versions' controls by Identifier. A shared
// identifier is Changed when the SHA-256 of its Statement differs. When an
// identifier repeats within one version, the last occurrence is compared.
// Result lists are sorted.
func DiffControls(prev, next []ControlRecord) ChangeReport {
	prevHashes := statementHashes(prev)
	nextHashes := statementHashes(next)

	var report ChangeReport
	shared := 0
	for id, h := range nextHashes {
		ph, ok := prevHashes[id]
		switch {
		case !ok:
			report.Added = append(report.Added, id)
		case ph != h:
			shared++
			report.Changed = append(report.Changed, id)
		default:
			shared++
		}
	}
	for id := range prevHashes {
		if _, ok := nextHashes[id]; !ok {
			report.Removed = append(report.Removed, id)
		}
	}
	sort.Strings(report.Added)
	sort.Strings(report.Removed)
	sort.Strings(report.Changed)

	larger := max(len(prevHashes), len(nextHashes))
	report.IDsUnstable = len(prevHashes) > 0 && len(nextHashes) > 0 &&
		shared*unstableIDShareDivisor < larger
	return report
}

func statementHashes(controls []ControlRecord) map[string][sha256.Size]byte {
	m := make(map[string][sha256.Size]byte, len(controls))
	for _, c := range controls {
		m[c.Identifier] = sha256.Sum256([]byte(c.Statement))
	}
	return m
}
