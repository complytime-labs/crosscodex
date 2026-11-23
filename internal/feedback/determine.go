package feedback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
)

// evidenceEntry is one evidence pair of a canonical request, which lists
// them sorted by key.
type evidenceEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// determinationRequest is the canonical request of a reviewer's
// determination. Its SHA-256 is the determination ID, and the signed
// feedback.determination link names it.
type determinationRequest struct {
	Tenant    string `json:"tenant"`
	JobID     string `json:"job_id"`
	MappingID string `json:"mapping_id"`
	// Verdict is the mapping's verdict key, "<job>:<source>:<target>".
	Verdict   string          `json:"verdict"`
	IsValid   bool            `json:"is_valid"`
	FinalType string          `json:"final_type"`
	Rationale string          `json:"rationale"`
	Evidence  []evidenceEntry `json:"evidence"`
	Reviewer  string          `json:"reviewer"`
	// PreviousDetermination is the review's determination this one
	// replaces, "" for none.
	PreviousDetermination string      `json:"previous_determination"`
	Aggregation           Aggregation `json:"aggregation"`
	// EdgeIDs are the edges the determination retires or restores.
	EdgeIDs   []string `json:"edge_ids"`
	DecidedAt string   `json:"decided_at"`
}

// sign encodes the canonical request req, hashes it into the determination
// ID and signs a link for step whose material is the request and whose
// product is the mapping. It returns the ID, the request and the link.
func (s *Service) sign(ctx context.Context, step, mappingID string, req any) (string, []byte, []byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", nil, nil, fmt.Errorf("encode the request: %w", err)
	}
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	link, err := s.signer.CreateLink(ctx, step,
		[]attestation.Artifact{{URI: "crosscodex:mapping-determination/" + id, Digest: id}},
		[]attestation.Artifact{{URI: "crosscodex:mapping/" + mappingID, Digest: mappingID}})
	if err != nil {
		return "", nil, nil, fmt.Errorf("sign the determination: %w", err)
	}
	return id, raw, link.Raw, nil
}

// RecordDetermination records the caller's determination that a mapping is
// valid or invalid, signs it, and applies its graph change: an invalid one
// retires the verdict's live edges, a valid one after an invalid one
// restores them, any other valid one changes no edge. Determinations
// recorded earlier but not yet applied are applied first.
func (s *Service) RecordDetermination(ctx context.Context, req *connect.Request[pb.RecordDeterminationRequest]) (_ *connect.Response[pb.RecordDeterminationResponse], err error) {
	ctx, finish := s.begin(ctx, "RecordDetermination", "record_determination")
	defer func() { finish(err) }()

	identity, err := requireRole(ctx, determineDenied, authn.RoleReviewer)
	if err != nil {
		return nil, err
	}
	ctx, tenantID, err := scopeCtx(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if s.g == nil || s.verdicts == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrDeterminationsUnavailable)
	}
	msg := req.Msg
	if err := checkRequestIDs(msg.GetJobId(), msg.GetMappingId()); err != nil {
		return nil, err
	}
	rationale := msg.GetDeterminationRationale()
	if strings.TrimSpace(rationale) == "" {
		return nil, invalid("determination_rationale is required: say why the mapping is valid or invalid")
	}
	if err := checkText("determination_rationale", rationale, maxRationaleBytes, true); err != nil {
		return nil, invalid("%w", err)
	}
	if err := checkEvidence(msg.GetEvidence()); err != nil {
		return nil, invalid("%w", err)
	}
	finalType, ok := relationshipName(msg.GetFinalType())
	if !ok {
		return nil, invalid("final_type %d is not a relationship type this server knows; leave it unset or set the mapping's own type", msg.GetFinalType())
	}

	m, err := s.lookupMapping(ctx, tenantID, msg.GetJobId(), msg.GetMappingId())
	if err != nil {
		return nil, serviceError(err)
	}
	if finalType != "" && finalType != m.RelationshipType {
		return nil, invalid("final_type %s is not mapping %s's type %s, and a determination cannot change a mapping's type. "+
			"Record is_valid=false and re-run the analysis; nothing was recorded", finalType, m.ID, m.RelationshipType)
	}

	resp, err := s.determine(ctx, tenantID, identity.Subject, m, msg.GetIsValid(), finalType, rationale, msg.GetEvidence())
	if err != nil {
		return nil, serviceError(err)
	}
	return connect.NewResponse(resp), nil
}

// determine records and applies one validated determination on m. See
// RecordDetermination.
func (s *Service) determine(ctx context.Context, tenantID, reviewer string, m Mapping, isValid bool, finalType, rationale string,
	evidence map[string]string) (*pb.RecordDeterminationResponse, error) {
	if applied, total, err := s.project(ctx, tenantID); err != nil {
		slog.ErrorContext(ctx, "feedback: apply outstanding mapping determinations", "tenant.id", tenantID,
			"applied", applied, "total", total, "error", err)
		return nil, fmt.Errorf("%d mapping determinations are recorded but not yet applied to the graph, so this determination on mapping %s "+
			"was refused and nothing was recorded. Check the daemon log for the graph error that stopped them, fix it, then retry: %w",
			total-applied, m.ID, ErrUnprojected)
	}
	if s.signer == nil {
		return nil, fmt.Errorf("record a determination on mapping %s: %w", m.ID, ErrSigningDisabled)
	}

	prev, reviewed, err := s.store.Review(ctx, tenantID, m.ID)
	if err != nil {
		return nil, fmt.Errorf("read mapping %s's review: %w", m.ID, err)
	}
	outcome, edges := ReviewValid, []string(nil)
	key := cleaning.VerdictKey{JobID: m.JobID, SourceID: m.SourceID, TargetID: m.TargetID}
	switch {
	case !isValid && reviewed && prev.Status == ReviewInvalid:
		return nil, fmt.Errorf("mapping %s is already invalid by determination %s; record is_valid=true to restore it. Nothing was recorded: %w",
			m.ID, prev.DeterminationID, ErrAlreadyInvalid)
	case !isValid:
		outcome = ReviewInvalid
		live, err := s.verdicts.VerdictEdges(ctx, tenantID, []cleaning.VerdictKey{key})
		if err != nil {
			return nil, fmt.Errorf("read mapping %s's live edges: %w", m.ID, err)
		}
		if edges = live[key]; len(edges) == 0 {
			return nil, fmt.Errorf("mapping %s has no live edge in the graph: edge cleaning retired it or its job's edges are gone; "+
				"see 'crosscodex results export %s'. Nothing was recorded: %w", m.ID, m.JobID, ErrNoLiveEdge)
		}
	case reviewed && prev.Status == ReviewInvalid:
		// The edges the invalid determination retired, to restore. A graph
		// rebuild may have removed them; a restore recorded then could never
		// be applied and would block every later determination.
		existing, err := cleaning.ExistingEdges(ctx, s.g, tenantID, prev.EdgeIDs)
		if err != nil {
			return nil, fmt.Errorf("read mapping %s's retired edges: %w", m.ID, err)
		}
		if len(existing) != len(prev.EdgeIDs) {
			return nil, fmt.Errorf("mapping %s's retired edges are no longer in the graph (it was rebuilt), so this restore was refused and nothing was recorded. "+
				"Run crosscodexd admin cleaning --reproject --tenant %s first: %w", m.ID, tenantID, ErrEdgesGone)
		}
		edges = prev.EdgeIDs
	}

	agg, err := s.aggregation(ctx, tenantID, m)
	if err != nil {
		return nil, err
	}
	decidedAt := s.stamp()
	keys := slices.Sorted(maps.Keys(evidence))
	entries := make([]evidenceEntry, len(keys))
	for i, k := range keys {
		entries[i] = evidenceEntry{Key: k, Value: evidence[k]}
	}
	id, raw, link, err := s.sign(ctx, "feedback.determination", m.ID, determinationRequest{
		Tenant: tenantID, JobID: m.JobID, MappingID: m.ID, Verdict: key.String(), IsValid: isValid, FinalType: finalType,
		Rationale: rationale, Evidence: entries, Reviewer: reviewer, PreviousDetermination: prev.DeterminationID,
		Aggregation: agg, EdgeIDs: edges, DecidedAt: decidedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, fmt.Errorf("record a determination on mapping %s: %w", m.ID, err)
	}
	d := Determination{
		ID: id, MappingID: m.ID, JobID: m.JobID, SourceID: m.SourceID, TargetID: m.TargetID, Outcome: outcome,
		DecidedBy: reviewer, Rationale: rationale, Request: raw, Attestation: link, DecidedAt: decidedAt,
	}
	if err := d.Validate(); err != nil {
		// Built from the request alone, so the whole message can be shown.
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	next := Review{
		MappingID: m.ID, JobID: m.JobID, SourceID: m.SourceID, TargetID: m.TargetID, Status: outcome,
		DeterminationID: id, DecidedAt: decidedAt, EdgeIDs: edges,
	}
	if err := s.store.Determine(ctx, tenantID, d, prev.DeterminationID, next); err != nil {
		return nil, fmt.Errorf("record a determination on mapping %s: %w", m.ID, err)
	}
	slog.InfoContext(ctx, "mapping determination recorded", "tenant.id", tenantID, "mapping", m.ID,
		"outcome", outcome, "determination", id, "edges", len(edges))

	if applied, total, err := s.project(ctx, tenantID); err != nil {
		slog.ErrorContext(ctx, "feedback: apply mapping determinations", "tenant.id", tenantID, "determination", id,
			"applied", applied, "total", total, "error", err)
		return nil, fmt.Errorf("determination %s on mapping %s was recorded and stands, but applying it to the graph stopped "+
			"(%d of %d recorded determinations applied). It is applied before the next determination is recorded; check the daemon log "+
			"for the graph error that stopped it, fix it, then record a determination or retry: %w", id, m.ID, applied, total, ErrUnprojected)
	}
	return &pb.RecordDeterminationResponse{DeterminationId: id, GraphUpdated: len(edges) > 0, MappingStatus: string(outcome)}, nil
}

// project applies every unprojected review's graph change and marks it
// projected, returning how many it applied of how many there were. An
// invalid review's edges are superseded at its decision time, and a valid
// review's stored edges (those an invalid one retired) are recreated under
// its determination, so a retry repeats the same change: retire skips
// superseded edges and restore finds the edges it already made. A review
// that changed since it was listed is left to its newer determination,
// which projects itself.
func (s *Service) project(ctx context.Context, tenantID string) (int, int, error) {
	uns, err := s.store.Unprojected(ctx, tenantID)
	if err != nil {
		return 0, 0, fmt.Errorf("list unprojected mapping determinations: %w", err)
	}
	for i, r := range uns {
		if err := s.projectOne(ctx, tenantID, r); err != nil {
			s.recordUnprojected(ctx, tenantID, len(uns)-i)
			return i, len(uns), err
		}
	}
	s.recordUnprojected(ctx, tenantID, 0)
	return len(uns), len(uns), nil
}

func (s *Service) projectOne(ctx context.Context, tenantID string, r Review) error {
	edges := r.EdgeIDs
	switch r.Status {
	case ReviewInvalid:
		if _, err := cleaning.RetireEdges(ctx, s.g, tenantID, r.EdgeIDs, r.DecidedAt); err != nil {
			return fmt.Errorf("mapping %s: apply determination %s: %w", r.MappingID, r.DeterminationID, err)
		}
	case ReviewValid:
		if len(r.EdgeIDs) > 0 {
			var err error
			if edges, err = cleaning.RestoreEdges(ctx, s.g, tenantID, r.EdgeIDs, r.DeterminationID, r.DecidedAt); err != nil {
				return fmt.Errorf("mapping %s: apply determination %s: %w", r.MappingID, r.DeterminationID, err)
			}
		}
	default:
		return fmt.Errorf("mapping %s's review has status %q, which no recorded determination leaves unapplied, so nothing was applied for it. "+
			"Its mapping_reviews row was changed outside crosscodex; restore it from the mapping's determinations", r.MappingID, r.Status)
	}
	err := s.store.MarkProjected(ctx, tenantID, r.MappingID, r.DeterminationID, edges)
	if err != nil && !errors.Is(err, ErrReviewChanged) {
		return fmt.Errorf("mapping %s: mark determination %s applied: %w", r.MappingID, r.DeterminationID, err)
	}
	return nil
}

// recordUnprojected sets feedback.reviews.unprojected to n for the tenant.
func (s *Service) recordUnprojected(ctx context.Context, tenantID string, n int) {
	if s.tel.unprojected != nil {
		s.tel.unprojected.Record(ctx, int64(n), metric.WithAttributes(attribute.String("tenant.id", tenantID)))
	}
}
