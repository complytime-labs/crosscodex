package feedback

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// Validation limits from the spec (#230).
const (
	maxRationaleBytes     = 4096
	maxEvidenceEntries    = 32
	maxEvidenceKeyBytes   = 128
	maxEvidenceValueBytes = 1024
	// maxIDBytes bounds IDs, voters and deciders, as pkg/cleaning bounds a
	// verdict key's parts.
	maxIDBytes = 256
	// maxStoredEdgeIDsBytes bounds the mapping_reviews.edge_ids JSON a store
	// writes. PostgreSQL holds at most 2^28-1 bytes in a jsonb value and
	// renders it with a space after every ',', so a list Go encodes within
	// half of that always reads back; pkg/cleaning bounds edge_findings'
	// lists the same way.
	maxStoredEdgeIDsBytes = 1 << 27
)

// storedEdgeIDsLimit is maxStoredEdgeIDsBytes. It is a variable only so that
// specs can lower it (export_test.go) instead of building a 128 MiB value;
// nothing else sets it.
var storedEdgeIDsLimit = maxStoredEdgeIDsBytes

// relationshipTypePrefix is the prefix of every pb.RelationshipType name; a
// suggested type is the name without it, as vote_summaries.consensus stores
// relationship types.
const relationshipTypePrefix = "RELATIONSHIP_TYPE_"

// jobStatusCompleted mirrors pipeline.JobStatusCompleted
// (internal/pipeline/types.go); internal packages do not import each other.
const jobStatusCompleted = "completed"

// DecidedByReproject is Determination.DecidedBy for re-projection's
// ReviewStale determination, and for no other.
const DecidedByReproject = cleaning.DecidedByReproject

// Sentinels a Store returns, wrapped with an actionable message.
var (
	// ErrInvalid marks a vote or determination Validate refuses.
	ErrInvalid = errors.New("invalid feedback")
	// ErrJobNotFound marks a job the tenant in ctx does not have, or cannot
	// see. Another tenant's job is reported the same way.
	ErrJobNotFound = errors.New("job not found")
	// ErrMappingNotFound marks a mapping that is not one of the job's.
	ErrMappingNotFound = errors.New("mapping not found")
	// ErrReviewNotFound marks a mapping with no review: no determination
	// was ever recorded for it.
	ErrReviewNotFound = errors.New("review not found")
	// ErrReviewChanged marks a write quoting a determination the review no
	// longer has: it changed since the caller read it.
	ErrReviewChanged = errors.New("review changed since it was read")
	// ErrUnprojected marks a review whose determination is recorded but
	// whose graph changes are not yet applied.
	ErrUnprojected = errors.New("review not yet applied to the graph")
	// ErrEdgeIDsTooLarge marks edge IDs whose JSON would exceed the bound
	// within which a stored review reads back.
	ErrEdgeIDsTooLarge = errors.New("edge IDs too large to store")
)

// checkEdgeIDs refuses edge IDs whose stored JSON would exceed
// storedEdgeIDsLimit; op names the write.
func checkEdgeIDs(op, mappingID string, ids []string) error {
	if n := len(edgeIDsJSON(ids)); n > storedEdgeIDsLimit {
		return fmt.Errorf("%s %s: %w: the edge_ids column would hold %d bytes of JSON, over the %d-byte limit within which a stored review reads back; "+
			"no mapping owns that many edges, so report this as a defect; nothing was recorded",
			op, mappingID, ErrEdgeIDsTooLarge, n, storedEdgeIDsLimit)
	}
	return nil
}

// checkRelationshipType refuses a name that is not a pb.RelationshipType
// name without its prefix, or is the unspecified one.
func checkRelationshipType(name string) error {
	n, ok := pb.RelationshipType_value[relationshipTypePrefix+name]
	if !ok || n == int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
		var names []string
		for full, v := range pb.RelationshipType_value {
			if v != int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
				names = append(names, strings.TrimPrefix(full, relationshipTypePrefix))
			}
		}
		slices.Sort(names)
		return fmt.Errorf("unknown relationship type %q; use one of %s", name, strings.Join(names, ", "))
	}
	return nil
}

// JobNotCompletedError means the job exists but has not completed, so its
// mappings are not final.
type JobNotCompletedError struct {
	JobID  string
	Status string
}

func (e *JobNotCompletedError) Error() string {
	return fmt.Sprintf("job %s is %s, not completed: its mappings are not final; wait for it to complete", e.JobID, e.Status)
}

// MappingID returns the ID of the job's mapping of sourceID to targetID: the
// SEMANTIC_MATCH edge ID the job wrote for the pair.
func MappingID(jobID, sourceID, targetID string) string {
	return graphdb.DerivedID(analyzerresults.SemanticMatchEdgeIDKind, jobID, sourceID, targetID)
}

// Mapping is one synthesized mapping of a completed job: its relationship
// analyzer's vote_summaries row. RelationshipType is the row's consensus.
type Mapping struct {
	ID, JobID, SourceID, TargetID, RelationshipType string
	Confidence, Viability                           float64
}

// VoteType is how a reviewer voted, as mapping_votes.vote_type stores it.
type VoteType string

// Vote types.
const (
	VoteAgree    VoteType = "agree"
	VoteDisagree VoteType = "disagree"
	VoteRefine   VoteType = "refine"
	VoteEscalate VoteType = "escalate"
)

var voteTypes = []VoteType{VoteAgree, VoteDisagree, VoteRefine, VoteEscalate}

// Vote is one append-only mapping_votes row.
type Vote struct {
	ID, MappingID, JobID, SourceID, TargetID, Voter string
	Type                                            VoteType
	// SuggestedType is the relationship a VoteRefine proposes; only a
	// refine vote has one, and it must.
	SuggestedType string
	Rationale     string
	Evidence      map[string]string
	// CreatedAt is stored at microsecond precision; the driver truncates
	// finer times.
	CreatedAt time.Time
}

// ReviewStatus is a mapping's reviewed state, as mapping_reviews.status and
// mapping_determinations.outcome store it.
type ReviewStatus string

// Review statuses.
const (
	ReviewValid   ReviewStatus = "valid"
	ReviewInvalid ReviewStatus = "invalid"
	// ReviewStale is recorded only by re-projection (MarkStale), always by
	// DecidedByReproject, when a reviewed mapping's edges are gone from a
	// rebuilt graph.
	ReviewStale ReviewStatus = "stale"
)

var reviewStatuses = []ReviewStatus{ReviewValid, ReviewInvalid, ReviewStale}

// Review is one mapping_reviews row: a mapping's current determination.
type Review struct {
	MappingID, JobID, SourceID, TargetID string
	Status                               ReviewStatus
	// DeterminationID and DecidedAt are the current determination's.
	DeterminationID string
	DecidedAt       time.Time
	// EdgeIDs are the graph edges the determination changed; a store returns
	// none as nil.
	EdgeIDs []string
	// GraphApplied is false while those edge changes are not yet written.
	GraphApplied bool
}

// Determination is one append-only mapping_determinations row.
type Determination struct {
	// ID is the hex SHA-256 of Request, the canonical request.
	ID, MappingID, JobID, SourceID, TargetID string
	Outcome                                  ReviewStatus
	// DecidedBy is a reviewer ID, or DecidedByReproject for ReviewStale.
	DecidedBy, Rationale string
	// Request and Attestation (its signed in-toto link) are stored and
	// returned byte for byte.
	Request, Attestation []byte
	// DecidedAt is stored at microsecond precision; the driver truncates
	// finer times.
	DecidedAt time.Time
}

// checkText refuses s unless it is valid UTF-8 without control characters
// (multiline allows '\n' and '\t') and, when maxBytes is positive, at most
// maxBytes bytes long.
func checkText(field, s string, maxBytes int, multiline bool) error {
	if maxBytes > 0 && len(s) > maxBytes {
		return fmt.Errorf("%s is %d bytes, over the %d-byte limit; shorten it", field, len(s), maxBytes)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8; send it as UTF-8 text", field)
	}
	for _, r := range s {
		whitespace := multiline && (r == '\n' || r == '\t')
		if unicode.IsControl(r) && !whitespace {
			if multiline {
				return fmt.Errorf("%s holds control character %U; only newlines and tabs are allowed, so remove it", field, r)
			}
			return fmt.Errorf("%s holds control character %U; remove it", field, r)
		}
	}
	return nil
}

// checkMapping refuses a record whose IDs are empty or not text, or whose
// mapping ID is not MappingID of its job and pair.
func checkMapping(mappingID, jobID, sourceID, targetID string) error {
	for _, f := range []struct{ name, value string }{
		{"mapping ID", mappingID}, {"job ID", jobID}, {"source ID", sourceID}, {"target ID", targetID},
	} {
		if f.value == "" {
			return fmt.Errorf("the %s is required", f.name)
		}
		limit := maxIDBytes
		if f.name == "mapping ID" {
			// Derived from the three bounded parts; checked by equality below.
			limit = 0
		}
		if err := checkText(f.name, f.value, limit, false); err != nil {
			return err
		}
	}
	if want := MappingID(jobID, sourceID, targetID); mappingID != want {
		return fmt.Errorf("mapping ID %q does not name job %s's mapping of %s to %s (%q); take mapping IDs from the job's results",
			mappingID, jobID, sourceID, targetID, want)
	}
	return nil
}

// Validate reports whether a store may record v: non-empty IDs and voter, a
// known type, a suggested type with and only with VoteRefine (a
// pb.RelationshipType name without its RELATIONSHIP_TYPE_ prefix, never
// UNSPECIFIED), IDs and voter of at most 256 bytes, a rationale
// (optional for VoteAgree) of at most 4096 bytes, at most 32 evidence
// entries with keys of at most 128 bytes and values of at most 1024, a
// creation time, and a mapping ID that is MappingID of its job and pair. All
// strings must be valid UTF-8 without control characters; the rationale may
// hold newlines and tabs. Errors wrap ErrInvalid.
func (v Vote) Validate() error {
	if err := v.validate(); err != nil {
		return fmt.Errorf("vote %s: %w; nothing was recorded: %w", v.ID, err, ErrInvalid)
	}
	return nil
}

func (v Vote) validate() error {
	if v.ID == "" || v.Voter == "" {
		return errors.New("a vote ID and a voter are required")
	}
	if err := checkText("vote ID", v.ID, maxIDBytes, false); err != nil {
		return err
	}
	if err := checkText("voter", v.Voter, maxIDBytes, false); err != nil {
		return err
	}
	if err := checkMapping(v.MappingID, v.JobID, v.SourceID, v.TargetID); err != nil {
		return err
	}
	if !slices.Contains(voteTypes, v.Type) {
		return fmt.Errorf("unknown vote type %q; use %q, %q, %q or %q", v.Type, VoteAgree, VoteDisagree, VoteRefine, VoteEscalate)
	}
	switch {
	case v.Type == VoteRefine && v.SuggestedType == "":
		return errors.New("a refine vote must suggest the relationship type it proposes")
	case v.Type != VoteRefine && v.SuggestedType != "":
		return fmt.Errorf("only a refine vote suggests a relationship type, and this is a %s vote; vote refine or drop the suggested type", v.Type)
	}
	if v.Type == VoteRefine {
		if err := checkRelationshipType(v.SuggestedType); err != nil {
			return err
		}
	}
	if v.Type != VoteAgree && strings.TrimSpace(v.Rationale) == "" {
		return fmt.Errorf("%s votes need a rationale: say why", v.Type)
	}
	if err := checkText("rationale", v.Rationale, maxRationaleBytes, true); err != nil {
		return err
	}
	if err := checkEvidence(v.Evidence); err != nil {
		return err
	}
	if v.CreatedAt.IsZero() {
		return errors.New("a creation time is required")
	}
	return nil
}

// checkEvidence refuses more than 32 evidence entries, keys over 128 bytes,
// values over 1024 bytes, and keys or values that are not text.
func checkEvidence(evidence map[string]string) error {
	if len(evidence) > maxEvidenceEntries {
		return fmt.Errorf("evidence has %d entries, over the limit of %d; cite fewer", len(evidence), maxEvidenceEntries)
	}
	for _, k := range slices.Sorted(maps.Keys(evidence)) {
		if err := checkText(fmt.Sprintf("evidence key %q", k), k, maxEvidenceKeyBytes, false); err != nil {
			return err
		}
		if err := checkText(fmt.Sprintf("evidence value of %q", k), evidence[k], maxEvidenceValueBytes, false); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports whether a store may record d: non-empty IDs and decider
// of at most 256 bytes,
// a known outcome, ReviewStale decided by DecidedByReproject and by nothing
// else, a non-blank rationale of at most 4096 bytes (UTF-8 without control
// characters but newlines and tabs), a request and an attestation, a
// decision time, and a mapping ID that is MappingID of its job and pair.
// Errors wrap ErrInvalid.
func (d Determination) Validate() error {
	if err := d.validate(); err != nil {
		return fmt.Errorf("determination %s: %w; nothing was recorded: %w", d.ID, err, ErrInvalid)
	}
	return nil
}

func (d Determination) validate() error {
	if d.ID == "" || d.DecidedBy == "" {
		return errors.New("a determination ID and a decider are required")
	}
	if err := checkText("determination ID", d.ID, maxIDBytes, false); err != nil {
		return err
	}
	if err := checkText("decider", d.DecidedBy, maxIDBytes, false); err != nil {
		return err
	}
	if err := checkMapping(d.MappingID, d.JobID, d.SourceID, d.TargetID); err != nil {
		return err
	}
	if !slices.Contains(reviewStatuses, d.Outcome) {
		return fmt.Errorf("unknown outcome %q; use %q or %q", d.Outcome, ReviewValid, ReviewInvalid)
	}
	if (d.Outcome == ReviewStale) != (d.DecidedBy == DecidedByReproject) {
		return fmt.Errorf("outcome %q decided by %q: only re-projection records a %q determination, always as %q, and it records no other outcome",
			d.Outcome, d.DecidedBy, ReviewStale, DecidedByReproject)
	}
	if strings.TrimSpace(d.Rationale) == "" {
		return errors.New("a rationale is required: say why the mapping is valid or invalid")
	}
	if err := checkText("rationale", d.Rationale, maxRationaleBytes, true); err != nil {
		return err
	}
	if len(d.Request) == 0 || len(d.Attestation) == 0 {
		return errors.New("a request and an attestation are required so the determination can be verified later")
	}
	if d.DecidedAt.IsZero() {
		return errors.New("a decision time is required")
	}
	return nil
}
