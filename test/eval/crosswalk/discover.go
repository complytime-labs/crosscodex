package crosswalk

import (
	"cmp"
	"slices"
	"strings"
)

// DiscoverRow reports, for one identifier mode, how well the identifiers drawn
// from one (link rel, back-matter resource) pair resolve against the target.
type DiscoverRow struct {
	Rel           string
	ResourceUUID  string
	ResourceTitle string
	// Mode is a profile identifier mode: "link_text" or "resource_title".
	Mode string
	// Links is the number of source links with this rel citing the resource.
	Links int
	// Identifiers counts identifiers the mode extracts from those links.
	Identifiers int
	// Resolved counts identifiers that resolve to exactly one target control.
	Resolved int
}

type discoverKey struct{ rel, uuid string }

// Discover scans every source control's "#uuid" links and reports, per
// (rel, resource) and per identifier mode, the share of identifiers that
// resolve against idx. It helps write a profile and is advisory only: links
// whose fragment names no back-matter resource are skipped (not counted in any
// row) rather than treated as an error. link_text mode splits with the default
// separator. Rows sort by Resolved/Identifiers descending, then title, then
// rel, UUID and mode so the order is deterministic.
func Discover(src *Catalog, idx *TargetIndex) []DiscoverRow {
	titles := map[string]string{}
	if bm := src.Doc.BackMatter; bm != nil && bm.Resources != nil {
		for _, r := range *bm.Resources {
			titles[r.UUID] = r.Title
		}
	}

	rows := map[discoverKey]*[2]DiscoverRow{}
	var order []discoverKey
	for _, ctrl := range src.Controls() {
		if ctrl.Links == nil {
			continue
		}
		for _, link := range *ctrl.Links {
			if !strings.HasPrefix(link.Href, "#") {
				continue
			}
			id := strings.TrimPrefix(link.Href, "#")
			title, ok := titles[id]
			if !ok {
				continue
			}
			k := discoverKey{link.Rel, id}
			pair, seen := rows[k]
			if !seen {
				pair = &[2]DiscoverRow{
					{Rel: link.Rel, ResourceUUID: id, ResourceTitle: title, Mode: identifierLinkText},
					{Rel: link.Rel, ResourceUUID: id, ResourceTitle: title, Mode: identifierResourceTitle},
				}
				rows[k] = pair
				order = append(order, k)
			}
			var textIDs, titleIDs []string
			textIDs = SplitLinkText(link.Text, defaultSeparator)
			if t := strings.TrimSpace(title); t != "" {
				titleIDs = []string{t}
			}
			for i, idents := range [][]string{textIDs, titleIDs} {
				pair[i].Links++
				pair[i].Identifiers += len(idents)
				for _, ident := range idents {
					if idx.Resolve(ident).State == Resolved {
						pair[i].Resolved++
					}
				}
			}
		}
	}

	var out []DiscoverRow
	for _, k := range order {
		out = append(out, rows[k][0], rows[k][1])
	}
	share := func(r DiscoverRow) float64 {
		if r.Identifiers == 0 {
			return 0
		}
		return float64(r.Resolved) / float64(r.Identifiers)
	}
	slices.SortStableFunc(out, func(a, b DiscoverRow) int {
		return cmp.Or(
			cmp.Compare(share(b), share(a)),
			cmp.Compare(a.ResourceTitle, b.ResourceTitle),
			cmp.Compare(a.Rel, b.Rel),
			cmp.Compare(a.ResourceUUID, b.ResourceUUID),
			cmp.Compare(a.Mode, b.Mode),
		)
	})
	return out
}
