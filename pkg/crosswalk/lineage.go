package crosswalk

import (
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// LineageSegment renders one ancestor of a control for a breadcrumb: "title
// (label)", else "title (identifier)", else "title". A blank (empty or
// whitespace-only) label or identifier counts as absent. A title that carries
// no text beyond the label or identifier (oscal.UsableTitle) is rendered
// alone, so "MANAGE" labelled MANAGE is not "MANAGE (MANAGE)". It returns ""
// when the title is blank, so an untitled ancestor drops out of the
// breadcrumb.
func LineageSegment(title, label, identifier string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	if oscal.UsableTitle(title, identifier, label) == "" {
		return title
	}
	for _, mark := range []string{label, identifier} {
		if mark = strings.TrimSpace(mark); mark != "" {
			return title + " (" + mark + ")"
		}
	}
	return title
}
