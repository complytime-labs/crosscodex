package classify

import (
	"errors"
	"strings"
)

// ErrEmptyInput is returned when ParseClassification receives an empty or
// whitespace-only input string.
var ErrEmptyInput = errors.New("empty classification response")

// ParseClassification parses an LLM response string into a classification Result.
//
// The expected format is "Type|Level" (e.g., "Technical|Operational").
// Parsing is lenient: case-insensitive, strips whitespace/periods/newlines,
// uses substring matching, and defaults unknown values to safe fallbacks
// (None for type, Tactical for level). If type is None, level is forced
// to None regardless of the parsed value.
//
// Returns ErrEmptyInput only for empty strings after stripping. All other
// inputs produce a valid Result (fail-closed to None|None for unrecognizable input).
func ParseClassification(raw string) (Result, error) {
	cleaned := cleanResponse(raw)
	if cleaned == "" {
		return Result{}, ErrEmptyInput
	}

	rawType, rawLevel := splitPipe(cleaned)
	typ := normalizeType(rawType)
	level := normalizeLevel(rawLevel)

	// None type forces None level regardless of parsed value.
	if typ == TypeNone {
		level = LevelNone
	}

	return Result{Type: typ, Level: level}, nil
}

// cleanResponse strips whitespace, newlines, carriage returns, and trailing periods.
func cleanResponse(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.TrimRight(s, ".")
	s = strings.TrimSpace(s)
	return s
}

// splitPipe splits on the first "|". If no pipe, returns (input, "") which
// normalizeLevel will handle by defaulting to Tactical. Extra pipe-separated
// segments beyond the second are silently discarded (e.g., "A|B|C" → "A", "B").
func splitPipe(s string) (string, string) {
	parts := strings.SplitN(s, "|", 3) // limit to 3 to discard extra segments
	if len(parts) < 2 {
		return s, ""
	}
	return parts[0], parts[1]
}

// normalizeType performs case-insensitive substring matching on the type portion.
// Order matters: "both" is checked before "none" to avoid false matches.
// Unknown inputs fall through to TypeNone (fail-closed).
func normalizeType(s string) ClassificationType {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.Contains(lower, "technical"):
		return TypeTechnical
	case strings.Contains(lower, "procedural"):
		return TypeProcedural
	case strings.Contains(lower, "both"):
		return TypeBoth
	case strings.Contains(lower, "none"), strings.Contains(lower, "noise"):
		return TypeNone
	default:
		return TypeNone
	}
}

// normalizeLevel performs case-insensitive substring matching on the level portion.
// Default is Tactical for non-empty input, matching the Python behavior.
// Empty input (from missing pipe) also defaults to Tactical.
func normalizeLevel(s string) ClassificationLevel {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return LevelTactical
	}
	switch {
	case strings.Contains(lower, "strategic"):
		return LevelStrategic
	case strings.Contains(lower, "operational"):
		return LevelOperational
	case strings.Contains(lower, "tactical"):
		return LevelTactical
	case strings.Contains(lower, "none"):
		return LevelNone
	default:
		return LevelTactical
	}
}
