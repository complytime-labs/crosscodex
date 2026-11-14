package analyzer

import (
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Analysable reports whether a control is worth analyzing: not Excluded and
// carrying statement text. Every analyzer and the embedding path use this one
// rule so that they skip the same controls.
func Analysable(c *pb.Control) bool {
	return !Excluded(c) && strings.TrimSpace(c.GetStatement()) != ""
}

// Excluded reports whether a control is never analysed whatever its text: a
// structural section or a withdrawn control. Candidate generation filters on
// it alone so that a control without text still reaches its blank-text count.
func Excluded(c *pb.Control) bool {
	parts := c.GetParts()
	return parts["class"] == oscal.ClassSection || parts[oscal.PropStatus] == oscal.PropStatusWithdrawn
}
