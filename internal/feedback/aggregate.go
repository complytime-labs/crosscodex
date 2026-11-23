package feedback

import (
	"context"
	"fmt"
	"math"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// Aggregation is what reviewers' latest votes say about one mapping. A
// determination's canonical request records it, so the record shows what
// reviewers had said.
type Aggregation struct {
	Agree    int `json:"agree"`
	Disagree int `json:"disagree"`
	Refine   int `json:"refine"`
	Escalate int `json:"escalate"`
	// AgreementScore is Agree over all counted votes, 0 with none.
	AgreementScore float64 `json:"agreement_score"`
	// ConsensusType is the most suggested refine type when refine votes
	// outnumber every other type and one suggested type leads; else the
	// mapping's own type.
	ConsensusType string `json:"consensus_type"`
	// NeedsEscalation is set by any escalate vote, or by at least one vote
	// with AgreementScore below feedback.min_agreement.
	NeedsEscalation bool `json:"needs_escalation"`
}

// Votes is the number of votes counted.
func (a Aggregation) Votes() int { return a.Agree + a.Disagree + a.Refine + a.Escalate }

// aggregate counts votes, each reviewer's latest vote on m, as the spec's
// aggregation rule says (#230); minAgreement is feedback.min_agreement.
func aggregate(m Mapping, votes []Vote, minAgreement float64) Aggregation {
	a := Aggregation{ConsensusType: m.RelationshipType}
	suggested := map[string]int{}
	for _, v := range votes {
		switch v.Type {
		case VoteAgree:
			a.Agree++
		case VoteDisagree:
			a.Disagree++
		case VoteRefine:
			a.Refine++
			suggested[v.SuggestedType]++
		case VoteEscalate:
			a.Escalate++
		}
	}
	n := a.Votes()
	if n > 0 {
		a.AgreementScore = float64(a.Agree) / float64(n)
	}
	if a.Refine > a.Agree && a.Refine > a.Disagree && a.Refine > a.Escalate {
		best, lead, tied := "", 0, false
		for t, c := range suggested {
			switch {
			case c > lead:
				best, lead, tied = t, c, false
			case c == lead:
				tied = true
			}
		}
		if !tied {
			a.ConsensusType = best
		}
	}
	a.NeedsEscalation = a.Escalate > 0 || (n > 0 && a.AgreementScore < minAgreement)
	return a
}

// aggregation reads the latest votes on m and aggregates them.
func (s *Service) aggregation(ctx context.Context, tenantID string, m Mapping) (Aggregation, error) {
	votes, err := s.store.LatestVotes(ctx, tenantID, m.JobID)
	if err != nil {
		return Aggregation{}, fmt.Errorf("read job %s's votes: %w", m.JobID, err)
	}
	var mine []Vote
	for _, v := range votes {
		if v.MappingID == m.ID {
			mine = append(mine, v)
		}
	}
	return aggregate(m, mine, s.cfg.MinAgreement), nil
}

// count32 narrows a count for an int32 proto field, refusing one that does
// not fit rather than sending a wrong number.
func count32(name string, n int) (int32, error) {
	if n < 0 || n > math.MaxInt32 {
		return 0, fmt.Errorf("%s %d does not fit the response's int32 field", name, n)
	}
	return int32(n), nil
}

func aggregationToProto(mappingID string, a Aggregation) (*pb.VoteAggregation, error) {
	out := &pb.VoteAggregation{
		MappingId: mappingID, AgreementScore: float32(a.AgreementScore),
		ConsensusType: relationshipType(a.ConsensusType), NeedsEscalation: a.NeedsEscalation,
	}
	for _, c := range []struct {
		name string
		n    int
		dst  *int32
	}{
		{"agree votes", a.Agree, &out.AgreeCount},
		{"disagree votes", a.Disagree, &out.DisagreeCount},
		{"refine votes", a.Refine, &out.RefineCount},
		{"escalate votes", a.Escalate, &out.EscalateCount},
	} {
		v, err := count32(c.name, c.n)
		if err != nil {
			return nil, err
		}
		*c.dst = v
	}
	return out, nil
}
