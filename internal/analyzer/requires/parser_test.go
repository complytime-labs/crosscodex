package requires_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
)

func TestParseResponse_Yes(t *testing.T) {
	raw := "REQUIRES: YES\nJUSTIFICATION: needs the target first\nCONFIDENCE: HIGH"
	vote := requires.ParseResponse("voter-1", raw)
	if vote.Decision == nil || !*vote.Decision {
		t.Fatalf("expected Decision=true, got %v", vote.Decision)
	}
	if vote.Confidence != "HIGH" {
		t.Errorf("Confidence: got %q, want HIGH", vote.Confidence)
	}
	if vote.Justification != "needs the target first" {
		t.Errorf("Justification: got %q", vote.Justification)
	}
}

func TestParseResponse_No(t *testing.T) {
	vote := requires.ParseResponse("voter-1", "REQUIRES: NO\nCONFIDENCE: MEDIUM")
	if vote.Decision == nil || *vote.Decision {
		t.Fatalf("expected Decision=false, got %v", vote.Decision)
	}
}

func TestParseResponse_Unparseable(t *testing.T) {
	vote := requires.ParseResponse("voter-1", "the model rambled without the expected format")
	if vote.Decision != nil {
		t.Fatalf("expected nil Decision for unparseable response, got %v", *vote.Decision)
	}
}

func TestParseResponse_DefaultsConfidenceToLowWhenAbsent(t *testing.T) {
	vote := requires.ParseResponse("voter-1", "REQUIRES: YES")
	if vote.Confidence != "LOW" {
		t.Errorf("Confidence: got %q, want LOW (default when no CONFIDENCE line present)", vote.Confidence)
	}
}
