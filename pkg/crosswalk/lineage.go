package crosswalk

import "strings"

// LineageSegment renders one ancestor of a control for a breadcrumb: "title
// (label)", else "title (identifier)", else "title". A blank (empty or
// whitespace-only) label or identifier counts as absent. It returns "" when
// the title is blank, so an untitled ancestor drops out of the breadcrumb.
func LineageSegment(title, label, identifier string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	for _, mark := range []string{label, identifier} {
		if mark = strings.TrimSpace(mark); mark != "" {
			return title + " (" + mark + ")"
		}
	}
	return title
}
