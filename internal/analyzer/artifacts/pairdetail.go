package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// expectLen is the length of an expect token in hex characters.
const expectLen = 16

// ErrPairChanged reports that the pair no longer holds the state a
// reviewer's expect token was taken from. DecideHuman returns it as a *PairChangedError.
var ErrPairChanged = errors.New("pair changed since it was viewed")

// PairChangedError is DecideHuman's refusal of a stale expect token. It
// describes the pair as it is now. DeterminedBy and DisplayBy are stored,
// untrusted text: sanitize them before display, and show DisplayBy.
type PairChangedError struct {
	Ref               string
	Status            VerdictStatus
	DeterminationType string
	DeterminedBy      string
	DisplayBy         string    // DeterminedBy, or its pseudonym when the reviewer is hidden
	DecidedAt         time.Time // zero: not decided
}

func (e *PairChangedError) Error() string {
	at := "never"
	if !e.DecidedAt.IsZero() {
		at = e.DecidedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("decide human %s: %v: it is now %s by %q (%s), decided %s; view the pair again and decide from its current EXPECT token",
		e.Ref, ErrPairChanged, e.Status, e.DisplayBy, e.DeterminationType, at)
}

// Is makes errors.Is(err, ErrPairChanged) match.
func (e *PairChangedError) Is(target error) bool { return target == ErrPairChanged }

// ValidateExpect checks an operator-supplied expect token, the EXPECT value
// `admin adjudicate artifacts show` prints.
func ValidateExpect(token string) error {
	if len(token) != expectLen || strings.Trim(token, "0123456789abcdef") != "" {
		return fmt.Errorf("expect token %q is not %d lowercase hex characters; copy the EXPECT line from \"crosscodexd admin adjudicate artifacts show\"", token, expectLen)
	}
	return nil
}

// VoteSplit counts a panel's votes: Same voted that the two names are one
// artifact, Valid votes parsed, Total were cast. Same is counted from the
// stored votes, while Valid and Total are copied from the stored evidence
// counts, so they need not agree with the votes.
type VoteSplit struct{ Same, Valid, Total int }

// Split counts e's votes.
func (e PanelEvidence) Split() VoteSplit {
	s := VoteSplit{Valid: e.ValidVotes, Total: e.TotalVotes}
	for _, v := range e.Votes {
		if v.Decision != nil && *v.Decision {
			s.Same++
		}
	}
	return s
}

// PanelDissent is the panel opinion stored on a human-owned pair: the
// verdict a human overrode, or one a panel gave after the human verdict.
type PanelDissent struct {
	Status       VerdictStatus
	Confidence   float64
	DeterminedBy string
	DecidedAt    time.Time
	Evidence     *PanelEvidence // nil when EvidenceErr is set
	EvidenceErr  error          // the stored dissent or its evidence could not be decoded
}

// RelatedPair is another pair that shares an artifact group with the one
// shown. The names are untrusted.
type RelatedPair struct {
	Ref               string
	Status            VerdictStatus
	DeterminationType string
	LowName, HighName string
}

// PairDetail is what a reviewer decides one pair from.
type PairDetail struct {
	ReviewItem
	DeterminedBy string
	DisplayBy    string    // DeterminedBy, or its pseudonym when the reviewer is hidden
	Confidence   *float64  // nil: not decided
	DecidedAt    time.Time // zero: not decided
	LastError    string
	// Evidence is the current llm_panel verdict's evidence: nil for other
	// determinations, or when EvidenceErr is set.
	Evidence    *PanelEvidence
	EvidenceErr error
	Dissent     *PanelDissent // nil: none
	// Disputed is true for a human verdict a panel contradicted afterwards,
	// the condition ReviewQueue lists it for.
	Disputed     bool
	Related      []RelatedPair
	RelatedTotal int // how many related pairs exist, not just len(Related)
	// Expect is the token HumanVerdict.Expect must carry to decide this pair.
	Expect string
}

// pairState is what a reviewer decides a pair from. Queue and projection
// bookkeeping (attempts, leases, last_error, next_attempt_at, graph_applied,
// the projection claim) is left out so background work never invalidates a
// token, and context is left out because a stored pair's context never
// changes.
type pairState struct {
	Status, DeterminationType, DeterminedBy string
	Confidence                              *float64  // nil: NULL
	DecidedAt                               time.Time // zero: NULL
	SimilarityScore                         float64
	Evidence                                []byte // stored JSON text; nil: NULL
	Dissent                                 []byte // stored JSON text; nil: NULL
}

// token is p's expect token: the first expectLen hex characters of SHA-256
// over every field, each written as "-;" when NULL or "<len>:<bytes>;"
// otherwise, so no two states share an encoding. decided_at is hashed at
// microsecond precision, the precision the database stores.
func (p pairState) token() string {
	h := sha256.New()
	field := func(present bool, b []byte) {
		if !present {
			fmt.Fprint(h, "-;")
			return
		}
		fmt.Fprintf(h, "%d:%s;", len(b), b)
	}
	float := func(f float64) []byte { return strconv.AppendFloat(nil, f, 'g', -1, 64) }
	field(true, []byte(p.Status))
	field(true, []byte(p.DeterminationType))
	field(true, []byte(p.DeterminedBy))
	if p.Confidence != nil {
		field(true, float(*p.Confidence))
	} else {
		field(false, nil)
	}
	field(!p.DecidedAt.IsZero(), strconv.AppendInt(nil, p.DecidedAt.UnixMicro(), 10))
	field(true, float(p.SimilarityScore))
	field(p.Evidence != nil, p.Evidence)
	field(p.Dissent != nil, p.Dissent)
	return hex.EncodeToString(h.Sum(nil))[:expectLen]
}
