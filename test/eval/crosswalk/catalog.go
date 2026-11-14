package crosswalk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// Catalog is a loaded OSCAL catalog and the identity of the file it came from.
type Catalog struct {
	Doc  *oscalTypes.Catalog
	Path string
	// Digest is the hex SHA-256 of the file bytes, so a score names the exact
	// catalog it was computed against.
	Digest string
}

// LoadCatalog reads and decodes the OSCAL JSON catalog at path.
func LoadCatalog(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("catalog %q not found: run task fetch:oscal-docs to download the catalogs: %w", path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("reading catalog %q: %w", path, err)
	}
	var root oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("decoding catalog %q as OSCAL JSON: %w", path, err)
	}
	if root.Catalog == nil {
		return nil, fmt.Errorf("%q has no top-level \"catalog\" object: it is not an OSCAL catalog", path)
	}
	sum := sha256.Sum256(raw)
	return &Catalog{Doc: root.Catalog, Path: path, Digest: hex.EncodeToString(sum[:])}, nil
}

// Controls returns every control, at any depth, depth-first in document order:
// groups (recursively) first, then the catalog's top-level controls.
func (c *Catalog) Controls() []oscalTypes.Control {
	var out []oscalTypes.Control
	var walk func(controls *[]oscalTypes.Control)
	var walkGroups func(groups *[]oscalTypes.Group)
	walk = func(controls *[]oscalTypes.Control) {
		if controls == nil {
			return
		}
		for _, ctrl := range *controls {
			out = append(out, ctrl)
			walk(ctrl.Controls)
		}
	}
	walkGroups = func(groups *[]oscalTypes.Group) {
		if groups == nil {
			return
		}
		for _, g := range *groups {
			walk(g.Controls)
			walkGroups(g.Groups)
		}
	}
	walkGroups(c.Doc.Groups)
	walk(c.Doc.Controls)
	return out
}
