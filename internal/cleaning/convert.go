package cleaning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// audit is the AuditMetadata of a response: the active span's trace ID.
func audit(ctx context.Context) *pb.AuditMetadata {
	return &pb.AuditMetadata{CorrelationId: telemetry.TraceIDFromContext(ctx)}
}

// count32 narrows a count for an int32 proto field, refusing one that does
// not fit rather than sending a wrong number.
func count32(name string, n int) (int32, error) {
	if n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%s %d does not fit the response's int32 field", name, n)
	}
	return int32(n), nil
}

// joinValues renders values as "a, b, c" for an error message.
func joinValues[T ~string](values []T) string {
	s := make([]string, len(values))
	for i, v := range values {
		s[i] = string(v)
	}
	return strings.Join(s, ", ")
}

// scopeFromProto converts a catalog pair scope, refusing one with a single
// side set: it would match no finding, which is never what the caller meant.
func scopeFromProto(s *pb.CatalogPairScope) (cleaning.Scope, error) {
	scope := cleaning.Scope{SourceCatalog: s.GetSourceCatalogId(), TargetCatalog: s.GetTargetCatalogId()}
	if (scope.SourceCatalog == "") != (scope.TargetCatalog == "") {
		return cleaning.Scope{}, invalid("scope has source_catalog_id %q and target_catalog_id %q: set both to limit the request "+
			"to one catalog pair, or neither to cover every pair", scope.SourceCatalog, scope.TargetCatalog)
	}
	return scope, nil
}

// refsFromProto parses retire_verdicts with the CLI's verdict-key rules, so
// the API accepts exactly the verdicts the CLI can name.
func refsFromProto(refs []*pb.VerdictRef) ([]cleaning.VerdictKey, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	keys := make([]cleaning.VerdictKey, len(refs))
	for i, r := range refs {
		k, err := cleaning.ParseVerdictKey(strings.Join([]string{r.GetJobId(), r.GetSourceNodeId(), r.GetTargetNodeId()}, ":"))
		if err != nil {
			return nil, invalid("retire_verdicts[%d]: %w; each of job_id, source_node_id and target_node_id must be set "+
				"and must not contain ':'. Take the verdicts from GetFinding", i, err)
		}
		keys[i] = k
	}
	return keys, nil
}

func refToProto(k cleaning.VerdictKey) *pb.VerdictRef {
	return &pb.VerdictRef{JobId: k.JobID, SourceNodeId: k.SourceID, TargetNodeId: k.TargetID}
}

// newFinding builds the proto finding shared by stored and planned findings.
func newFinding(id string, signal cleaning.Signal, status cleaning.Status, evidence cleaning.Evidence,
	targets []cleaning.VerdictKey, evidenceHash string) *pb.Finding {
	f := &pb.Finding{
		FindingId: id, Signal: string(signal), Status: string(status),
		Detail: evidence.Detail, EvidenceHash: evidenceHash,
	}
	for _, v := range evidence.Verdicts {
		f.Verdicts = append(f.Verdicts, &pb.VerdictEvidence{
			Ref:            refToProto(v.Key),
			JobCompletedAt: timestamppb.New(v.JobCompletedAt),
			Source: &pb.ControlRef{Identifier: v.SourceIdentifier, Label: v.SourceLabel,
				Title: v.SourceTitle, Catalog: v.SourceCatalog},
			Target: &pb.ControlRef{Identifier: v.TargetIdentifier, Label: v.TargetLabel,
				Title: v.TargetTitle, Catalog: v.TargetCatalog},
			Relationship: v.Relationship,
			Agreement:    v.Agreement,
			EdgeIds:      v.EdgeIDs,
		})
	}
	for _, k := range targets {
		f.Targets = append(f.Targets, refToProto(k))
	}
	if j := evidence.Job; j != nil {
		f.Job = &pb.FindingJob{
			JobId: j.ID, CompletedAt: timestamppb.New(j.CompletedAt),
			SourceCatalog: j.SourceCatalog, TargetCatalog: j.TargetCatalog, VerdictsInScope: int64(j.VerdictsInScope),
		}
	}
	return f
}

func findingToProto(s cleaning.StoredFinding) *pb.Finding {
	f := newFinding(s.ID, s.Signal, s.Status, s.Evidence, s.Targets, s.EvidenceHash)
	f.UpdatedAt = timestamppb.New(s.UpdatedAt)
	return f
}

// planToProto converts a preview's or an apply's plan. Status is the
// finding's status before the plan (empty for a new finding), to_status the
// one the plan gives it.
func planToProto(p cleaning.Plan) (*pb.RunCleaningResponse, error) {
	retire, err := count32("verdicts to retire", len(p.Retire))
	if err != nil {
		return nil, err
	}
	summary, err := summaryToProto(p.Summary)
	if err != nil {
		return nil, err
	}
	resp := &pb.RunCleaningResponse{PlanToken: p.Token, RetireVerdicts: retire, Summary: summary}
	for _, f := range p.Findings {
		resp.Findings = append(resp.Findings, &pb.PlannedFinding{
			Finding: newFinding(f.ID, f.Signal, f.From, f.Evidence, f.Targets, f.EvidenceHash),
			Action:  string(f.Action), Change: string(f.Change), ToStatus: string(f.To),
		})
	}
	return resp, nil
}

func summaryToProto(s cleaning.Summary) (*pb.CleaningSummary, error) {
	out := &pb.CleaningSummary{SkippedEdges: s.SkippedEdges}
	for _, c := range []struct {
		name string
		n    int
		dst  *int32
	}{
		{"verdicts", s.Verdicts, &out.Verdicts},
		{"pairs", s.Pairs, &out.Pairs},
		{"unknown_prompt", s.UnknownPrompt, &out.UnknownPrompt},
		{"unknown_model", s.UnknownModel, &out.UnknownModel},
		{"unaddressable_verdicts", s.UnaddressableVerdicts, &out.UnaddressableVerdicts},
	} {
		v, err := count32(c.name, c.n)
		if err != nil {
			return nil, err
		}
		*c.dst = v
	}
	return out, nil
}

func decisionToProto(d cleaning.Decision) *pb.Decision {
	sum := sha256.Sum256(d.Attestation)
	return &pb.Decision{
		DecisionId: d.ID, Verdict: d.Verdict, DecidedBy: d.DecidedBy, RunId: d.RunID, Rationale: d.Rationale,
		DecidedAt: timestamppb.New(d.DecidedAt), AttestationSha256: hex.EncodeToString(sum[:]),
	}
}
