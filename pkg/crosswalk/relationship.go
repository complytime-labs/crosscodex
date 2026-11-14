package crosswalk

import (
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// Direction says how a crosswalk row's relationship was obtained.
type Direction string

const (
	// Direct: a job judged the pair in the row's source-to-target direction.
	Direct Direction = "direct"
	// Derived: composed transitively through an intermediate control.
	Derived Direction = "derived"
	// Reverse: judged target-to-source by a relationship that cannot be
	// inverted (type unknown in the row's direction).
	Reverse Direction = "reverse"
)

// words are the user-facing relationship words, shared by every export format.
var words = map[pb.RelationshipType]string{
	pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT:      "equivalent",
	pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF:       "subset of",
	pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF:     "superset of",
	pb.RelationshipType_RELATIONSHIP_TYPE_CONTRIBUTES_TO:  "is advanced by",
	pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS:     "complements",
	pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL:         "partial",
	pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH:  "conflicts with",
	pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP: "no relationship",
}

// Word returns the user-facing word for t. Values without a fixed word fall
// back to the enum name lowercased, without prefix and with spaces for
// underscores; out-of-range values yield their numeric string.
func Word(t pb.RelationshipType) string {
	if w, ok := words[t]; ok {
		return w
	}
	name := strings.TrimPrefix(t.String(), "RELATIONSHIP_TYPE_")
	return strings.ReplaceAll(strings.ToLower(name), "_", " ")
}

// Invert returns the relationship that holds in the opposite direction.
// Subset and superset swap; symmetric types are unchanged. CONTRIBUTES_TO has
// no defined inverse, and UNSPECIFIED or unknown values cannot be reasoned
// about, so those return (t, false).
func Invert(t pb.RelationshipType) (pb.RelationshipType, bool) {
	switch t {
	case pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF:
		return pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF, true
	case pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF:
		return pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF, true
	case pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT,
		pb.RelationshipType_RELATIONSHIP_TYPE_COMPLEMENTS,
		pb.RelationshipType_RELATIONSHIP_TYPE_PARTIAL,
		pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH,
		pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP:
		return t, true
	default:
		return t, false
	}
}

// isRelated reports whether t is a defined relationship other than
// NO_RELATIONSHIP.
func isRelated(t pb.RelationshipType) bool {
	return t >= pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT &&
		t <= pb.RelationshipType_RELATIONSHIP_TYPE_CONFLICTS_WITH
}

// Compose returns the relationship A to C given A to B (ab) and B to C (bc).
// Only sound rules yield (type, true): EQUIVALENT is the identity, and
// SUBSET_OF / SUPERSET_OF are transitive. NO_RELATIONSHIP on either leg
// returns (NO_RELATIONSHIP, false) and callers drop the path. Any other pair
// returns (UNSPECIFIED, false): the controls are related but the type is
// unknown, and callers render "related (derived, type unknown)".
func Compose(ab, bc pb.RelationshipType) (pb.RelationshipType, bool) {
	const (
		unspecified = pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
		none        = pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP
		equivalent  = pb.RelationshipType_RELATIONSHIP_TYPE_EQUIVALENT
		subset      = pb.RelationshipType_RELATIONSHIP_TYPE_SUBSET_OF
		superset    = pb.RelationshipType_RELATIONSHIP_TYPE_SUPERSET_OF
	)
	switch {
	case ab == none || bc == none:
		return none, false
	case !isRelated(ab) || !isRelated(bc):
		return unspecified, false
	case ab == equivalent:
		return bc, true
	case bc == equivalent:
		return ab, true
	case ab == subset && bc == subset, ab == superset && bc == superset:
		return ab, true
	default:
		return unspecified, false
	}
}
