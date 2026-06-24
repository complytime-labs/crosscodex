package classify_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
)

func FuzzParseClassification(f *testing.F) {
	// Seed corpus: all canonical forms from ValidCombinations, plus edge cases
	// and attack vectors. ValidCombinations() produces 10 legal pairs
	// (None|None + 3 types x 3 non-None levels). We also seed all 16
	// type|level permutations to exercise the None-type forcing path.
	for _, t := range classify.AllTypes() {
		for _, l := range classify.AllLevels() {
			f.Add(t.String() + "|" + l.String())
		}
	}
	// Edge cases
	edgeCases := []string{
		"", " ", "|", "||", "|||",
		"noise", "Noise|Tactical",
		"TECHNICAL|OPERATIONAL",
		"  Technical|Operational  ",
		"Technical|Operational.",
		"Technical|Operational\n",
		"Technical|Operational\r\n",
		"Technical",
		// Unicode
		"\u0000", "\xff\xfe",
		"Technisch|Operational",
		"procedural|策略的",
	}
	for _, s := range edgeCases {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		result, err := classify.ParseClassification(raw)
		if err != nil {
			return // Empty input is expected to error; no further assertions.
		}
		// Invariants that must hold for all non-error results:
		if !result.Type.Valid() {
			t.Fatalf("invalid Type %d for input %q", result.Type, raw)
		}
		if !result.Level.Valid() {
			t.Fatalf("invalid Level %d for input %q", result.Level, raw)
		}
		if result.Type == classify.TypeNone && result.Level != classify.LevelNone {
			t.Fatalf("None type with non-None level %s for input %q", result.Level, raw)
		}
	})
}
