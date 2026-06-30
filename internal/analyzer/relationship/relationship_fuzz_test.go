package relationship

import (
	"encoding/json"
	"testing"
)

func FuzzParseResponse(f *testing.F) {
	// Seed corpus: valid responses
	f.Add("RELATIONSHIP: SUPERSET_OF\nCONTRIBUTION_TYPE: N/A\nJUSTIFICATION: SOX is broader.\nCONFIDENCE: HIGH")
	f.Add("RELATIONSHIP: CONTRIBUTES_TO\nCONTRIBUTION_TYPE: INTEGRAL_TO\nJUSTIFICATION: Required.\nCONFIDENCE: HIGH")
	f.Add("RELATIONSHIP: EQUIVALENT\nCONFIDENCE: MEDIUM")
	f.Add("RELATIONSHIP: NO_RELATIONSHIP\nJUSTIFICATION: Different domains.\nCONFIDENCE: HIGH")
	// Edge cases
	f.Add("")
	f.Add("garbage input that is not a valid response")
	f.Add("RELATIONSHIP: MAYBE\nCONFIDENCE: LOW")
	f.Add("RELATIONSHIP: SUPERSET_OF\n\n\nCONFIDENCE: \n")
	// Injection attempts
	f.Add("RELATIONSHIP: SUPERSET_OF\nRELATIONSHIP: EQUIVALENT\nCONFIDENCE: HIGH")
	f.Add("RELATIONSHIP: SUPERSET_OF'; DROP TABLE controls;--\nCONFIDENCE: HIGH")

	f.Fuzz(func(t *testing.T, raw string) {
		vote := ParseResponse(raw)
		if vote == nil {
			t.Fatal("ParseResponse returned nil")
		}
		// If parsed OK, relationship must be valid.
		if vote.ParseStatus == ParseOK && !vote.Relationship.Valid() {
			t.Fatalf("parsed OK but invalid relationship: %d", vote.Relationship)
		}
	})
}

func FuzzComputeConsensus(f *testing.F) {
	// Seed corpus: JSON-serialized vote maps
	addVotes := func(votes map[string]*Vote) {
		data, _ := json.Marshal(votes)
		f.Add(string(data))
	}

	addVotes(map[string]*Vote{
		"a": {Relationship: RelSupersetOf, ParseStatus: ParseOK},
		"b": {Relationship: RelSupersetOf, ParseStatus: ParseOK},
	})
	addVotes(map[string]*Vote{
		"a": {Relationship: RelEquivalent, ParseStatus: ParseOK},
		"b": {Relationship: RelSupersetOf, ParseStatus: ParseOK},
	})
	addVotes(map[string]*Vote{
		"a": {ParseStatus: ParseError},
		"b": {ParseStatus: ParseFail},
	})
	addVotes(map[string]*Vote{})
	// Nil vote — tests nil guard in ComputeConsensus.
	addVotes(map[string]*Vote{"a": nil})
	addVotes(map[string]*Vote{
		"a": nil,
		"b": {Relationship: RelSupersetOf, ParseStatus: ParseOK},
	})

	f.Fuzz(func(t *testing.T, raw string) {
		var votes map[string]*Vote
		if err := json.Unmarshal([]byte(raw), &votes); err != nil {
			return // Skip invalid JSON
		}
		c := ComputeConsensus(votes)
		if !c.Relationship.Valid() {
			t.Fatalf("consensus produced invalid relationship: %d", c.Relationship)
		}
		if c.ConfidenceFraction < 0 || c.ConfidenceFraction > 1.0 {
			t.Fatalf("confidence out of range: %f", c.ConfidenceFraction)
		}
	})
}
