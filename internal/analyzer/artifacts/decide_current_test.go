package artifacts_test

import (
	"context"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

// staleExpect is a well-formed expect token that matches no pair state.
const staleExpect = "0000000000000000"

// decideCurrent records h on the pair with ref as a reviewer who has just
// viewed it: Expect comes from PairDetail. If PairDetail fails (an unknown
// or malformed ref, a tenant mismatch), it passes staleExpect, so
// DecideHuman reports its own error for that input; specs of those errors
// therefore still see them.
func decideCurrent(ctx context.Context, s artifacts.VerdictStore, tenantID, ref string, h artifacts.HumanVerdict) (artifacts.HumanDecision, error) {
	h.Expect = staleExpect
	if d, err := s.PairDetail(ctx, tenantID, ref, 1); err == nil {
		h.Expect = d.Expect
	}
	return s.DecideHuman(ctx, tenantID, ref, h)
}
