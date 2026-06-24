package classify

import "fmt"

// ClassificationType represents the implementation type dimension.
type ClassificationType int

const (
	TypeNone       ClassificationType = iota // Not a requirement
	TypeTechnical                            // Software/hardware/automated
	TypeProcedural                           // Policy/process/human
	TypeBoth                                 // Requires both
)

// String returns the canonical string form used in proto attributes
// and LLM response comparison.
func (t ClassificationType) String() string {
	switch t {
	case TypeTechnical:
		return "Technical"
	case TypeProcedural:
		return "Procedural"
	case TypeBoth:
		return "Both"
	default:
		return "None"
	}
}

// Valid reports whether t is a defined ClassificationType value.
func (t ClassificationType) Valid() bool {
	return t >= TypeNone && t <= TypeBoth
}

// ClassificationLevel represents the abstraction level dimension.
type ClassificationLevel int

const (
	LevelNone        ClassificationLevel = iota // Only when Type is None
	LevelStrategic                              // WHAT, not HOW
	LevelTactical                               // HOW at process/control level
	LevelOperational                            // Exact step or config
)

// String returns the canonical string form used in proto attributes.
func (l ClassificationLevel) String() string {
	switch l {
	case LevelStrategic:
		return "Strategic"
	case LevelTactical:
		return "Tactical"
	case LevelOperational:
		return "Operational"
	default:
		return "None"
	}
}

// Valid reports whether l is a defined ClassificationLevel value.
func (l ClassificationLevel) Valid() bool {
	return l >= LevelNone && l <= LevelOperational
}

// AllTypes returns every defined ClassificationType in declaration order.
func AllTypes() []ClassificationType {
	return []ClassificationType{TypeNone, TypeTechnical, TypeProcedural, TypeBoth}
}

// AllLevels returns every defined ClassificationLevel in declaration order.
func AllLevels() []ClassificationLevel {
	return []ClassificationLevel{LevelNone, LevelStrategic, LevelTactical, LevelOperational}
}

// ValidCombinations returns every legal (Type, Level) pair. TypeNone is only
// paired with LevelNone; all other types pair with all non-None levels.
func ValidCombinations() []Result {
	var out []Result
	for _, t := range AllTypes() {
		if t == TypeNone {
			out = append(out, Result{Type: TypeNone, Level: LevelNone})
			continue
		}
		for _, l := range AllLevels() {
			if l == LevelNone {
				continue
			}
			out = append(out, Result{Type: t, Level: l})
		}
	}
	return out
}

// Result holds the classification outcome for a single control.
type Result struct {
	ControlID string
	Type      ClassificationType
	Level     ClassificationLevel
	Skipped   bool // true if auto-classified (section, not LLM-classified)
}

// String returns the "Type|Level" format (e.g., "Technical|Operational").
func (r Result) String() string {
	return fmt.Sprintf("%s|%s", r.Type, r.Level)
}
