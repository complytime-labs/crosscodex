package crosswalk

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	"github.com/google/uuid"
)

const subsetTitleSuffix = " (eval subset)"

// Subset returns a new catalog holding only the listed controls, so a run over
// it costs a fixed number of analysis calls. Each listed control moves directly
// under its top-level group (or under the catalog when it has no group) and
// loses its child controls; everything else, including groups left empty, is
// dropped. Back-matter and metadata are kept as is, except the title, which is
// marked as a subset. The UUID is derived from the source UUID and the listed
// controls (OSCAL: a UUID changes when content changes), so the same source and
// controls always give byte-identical output. src is not modified.
func Subset(src *Catalog, controls []string) (*oscalTypes.Catalog, error) {
	known := map[string]bool{}
	for _, c := range src.Controls() {
		known[c.ID] = true
	}
	var unknown []string
	for _, id := range controls {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, fmt.Errorf("controls not in source catalog %q: %s. Edit the profile's controls list to use control IDs that appear in that catalog file (the OSCAL control \"id\" values, not their labels)",
			src.Path, strings.Join(slices.Compact(unknown), ", "))
	}

	raw, err := json.Marshal(src.Doc)
	if err != nil {
		return nil, fmt.Errorf("copying catalog %q: %w", src.Path, err)
	}
	var out oscalTypes.Catalog
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("copying catalog %q: %w", src.Path, err)
	}

	wanted := map[string]bool{}
	for _, id := range controls {
		wanted[id] = true
	}
	// collect appends, in document order, every wanted control under controls
	// (at any depth) to dst, without its children.
	var collect func(controls *[]oscalTypes.Control, dst *[]oscalTypes.Control)
	collect = func(controls *[]oscalTypes.Control, dst *[]oscalTypes.Control) {
		if controls == nil {
			return
		}
		for _, c := range *controls {
			children := c.Controls
			if wanted[c.ID] {
				c.Controls = nil
				*dst = append(*dst, c)
			}
			collect(children, dst)
		}
	}
	var collectGroup func(g oscalTypes.Group, dst *[]oscalTypes.Control)
	collectGroup = func(g oscalTypes.Group, dst *[]oscalTypes.Control) {
		collect(g.Controls, dst)
		if g.Groups != nil {
			for _, sub := range *g.Groups {
				collectGroup(sub, dst)
			}
		}
	}

	var groups []oscalTypes.Group
	if out.Groups != nil {
		for _, g := range *out.Groups {
			var kept []oscalTypes.Control
			collectGroup(g, &kept)
			if len(kept) > 0 {
				g.Groups = nil
				g.Controls = &kept
				groups = append(groups, g)
			}
		}
	}
	var top []oscalTypes.Control
	collect(out.Controls, &top)

	out.Groups, out.Controls = nil, nil
	if len(groups) > 0 {
		out.Groups = &groups
	}
	if len(top) > 0 {
		out.Controls = &top
	}
	sorted := slices.Sorted(slices.Values(controls))
	out.UUID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(src.Doc.UUID+"\n"+strings.Join(slices.Compact(sorted), "\n"))).String()
	out.Metadata.Title += subsetTitleSuffix
	return &out, nil
}
