package crosswalk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxProfileControls = 50
	maxRegexBytes      = 256

	identifierLinkText      = "link_text"
	identifierResourceTitle = "resource_title"
	defaultSeparator        = ","
)

// Profile declares one yardstick: which source controls are scored and where
// their reference mappings to the target catalog come from. Paths stay as
// written (repo-root relative); callers resolve them.
type Profile struct {
	Name       string        `yaml:"name"`
	Source     string        `yaml:"source"`
	Target     string        `yaml:"target"`
	Controls   []string      `yaml:"controls"`
	References ReferenceSpec `yaml:"references"`
	// Digest is the hex SHA-256 of the profile file bytes.
	Digest string `yaml:"-"`
}

// ReferenceSpec names exactly one source of reference pairs.
type ReferenceSpec struct {
	OSCALLinks *LinkSpec `yaml:"oscal_links"`
	// CSV is a path relative to the repository root; header
	// source_id,target_id[,citation].
	CSV string `yaml:"csv"`
}

// LinkSpec selects the source-catalog links that count as references. Which
// links count is declared, never guessed: other cited frameworks collide with
// target labels.
type LinkSpec struct {
	Rel      string           `yaml:"rel"`
	Resource ResourceSelector `yaml:"resource"`
	// Identifier is "link_text" (default) or "resource_title".
	Identifier string `yaml:"identifier"`
	// Separator splits link text; link_text only, default ",".
	Separator string `yaml:"separator"`
}

// ResourceSelector picks back-matter resources by RE2 regex. At least one
// field is set; every field that is set must match.
type ResourceSelector struct {
	Title string `yaml:"title"`
	Rlink string `yaml:"rlink"`
}

// LoadProfile reads a profile, rejecting unknown keys and validating its shape.
// It does not read the catalogs the profile names.
func LoadProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading profile %q: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p Profile
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("profile %q is empty", path)
		}
		return nil, fmt.Errorf("decoding profile %q: %w", path, err)
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("profile %q must be a single YAML document", path)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("invalid profile %q: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	p.Digest = hex.EncodeToString(sum[:])
	return &p, nil
}

func (p *Profile) validate() error {
	for field, v := range map[string]string{"name": p.Name, "source": p.Source, "target": p.Target} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if n := len(p.Controls); n < 1 || n > maxProfileControls {
		return fmt.Errorf("controls has %d entries: it must list between 1 and %d source controls", n, maxProfileControls)
	}
	seen := map[string]bool{}
	for i, c := range p.Controls {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("controls[%d] is blank", i)
		}
		if seen[c] {
			return fmt.Errorf("controls[%d]: duplicate control %q", i, c)
		}
		seen[c] = true
	}
	if (p.References.OSCALLinks == nil) == (p.References.CSV == "") {
		return errors.New("references must set exactly one of references.oscal_links or references.csv")
	}
	if l := p.References.OSCALLinks; l != nil {
		return l.validate()
	}
	return nil
}

func (l *LinkSpec) validate() error {
	if strings.TrimSpace(l.Rel) == "" {
		return errors.New("references.oscal_links.rel is required")
	}
	if l.Resource.Title == "" && l.Resource.Rlink == "" {
		return errors.New("references.oscal_links.resource needs at least one of title or rlink")
	}
	for field, expr := range map[string]string{"title": l.Resource.Title, "rlink": l.Resource.Rlink} {
		if _, err := compileSelector(expr); err != nil {
			return fmt.Errorf("references.oscal_links.resource.%s: %w", field, err)
		}
	}
	switch l.Identifier {
	case "":
		l.Identifier = identifierLinkText
	case identifierLinkText, identifierResourceTitle:
	default:
		return fmt.Errorf("references.oscal_links.identifier %q is unknown: use %s or %s", l.Identifier, identifierLinkText, identifierResourceTitle)
	}
	if l.Identifier == identifierResourceTitle && l.Separator != "" {
		return fmt.Errorf("references.oscal_links.separator applies to %s only, not %s", identifierLinkText, identifierResourceTitle)
	}
	if l.Identifier == identifierLinkText && l.Separator == "" {
		l.Separator = defaultSeparator
	}
	return nil
}

// compileSelector compiles an RE2 expression after checking its length, so an
// untrusted profile cannot hand the compiler an oversized pattern. An empty
// expression yields a nil regexp, meaning "not set".
func compileSelector(expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	if len(expr) > maxRegexBytes {
		return nil, fmt.Errorf("regex is %d bytes, over the %d byte limit", len(expr), maxRegexBytes)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	return re, nil
}
