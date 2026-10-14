//go:build !integration

package artifacts_test

import (
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("pair state token", func() {
	conf := 0.5
	base := func() artifacts.ExportPairState {
		return artifacts.ExportPairState{Status: "confirmed", DeterminationType: "llm_panel", DeterminedBy: "artifact-adjudicator",
			Confidence: &conf, DecidedAt: time.Date(2026, 10, 1, 14, 2, 11, 123456000, time.UTC), SimilarityScore: 0.83,
			Evidence: []byte(`{"votes": []}`), Dissent: nil}
	}

	It("is 16 lowercase hex characters, accepted by ValidateExpect, and stable", func() {
		tok := artifacts.ExportPairStateToken(base())
		Expect(artifacts.ValidateExpect(tok)).To(Succeed())
		Expect(artifacts.ExportPairStateToken(base())).To(Equal(tok))
	})

	other := 0.75
	DescribeTable("changes when any decision field changes",
		func(mutate func(*artifacts.ExportPairState)) {
			p := base()
			mutate(&p)
			Expect(artifacts.ExportPairStateToken(p)).NotTo(Equal(artifacts.ExportPairStateToken(base())))
		},
		Entry("status", func(p *artifacts.ExportPairState) { p.Status = "rejected" }),
		Entry("determination type", func(p *artifacts.ExportPairState) { p.DeterminationType = "human" }),
		Entry("determined by", func(p *artifacts.ExportPairState) { p.DeterminedBy = "alice" }),
		Entry("confidence value", func(p *artifacts.ExportPairState) { p.Confidence = &other }),
		Entry("confidence NULL", func(p *artifacts.ExportPairState) { p.Confidence = nil }),
		Entry("decided at, by one microsecond", func(p *artifacts.ExportPairState) { p.DecidedAt = p.DecidedAt.Add(time.Microsecond) }),
		Entry("decided at NULL", func(p *artifacts.ExportPairState) { p.DecidedAt = time.Time{} }),
		Entry("similarity score", func(p *artifacts.ExportPairState) { p.SimilarityScore = 0.84 }),
		Entry("evidence", func(p *artifacts.ExportPairState) { p.Evidence = []byte(`{"votes": [1]}`) }),
		Entry("evidence NULL", func(p *artifacts.ExportPairState) { p.Evidence = nil }),
		Entry("dissent set", func(p *artifacts.ExportPairState) { p.Dissent = []byte(`{}`) }),
	)

	It("changes when only the dissent content changes", func() {
		a, b := base(), base()
		a.Dissent = []byte(`{"status": "rejected"}`)
		b.Dissent = []byte(`{"status": "confirmed"}`)
		Expect(artifacts.ExportPairStateToken(a)).NotTo(Equal(artifacts.ExportPairStateToken(b)))
	})

	It("ignores sub-microsecond differences, as the database stores microseconds", func() {
		p := base()
		p.DecidedAt = p.DecidedAt.Add(400 * time.Nanosecond)
		Expect(artifacts.ExportPairStateToken(p)).To(Equal(artifacts.ExportPairStateToken(base())))
	})

	It("length-prefixes fields, so moving bytes between adjacent fields changes the token", func() {
		a, b := base(), base()
		a.Status, a.DeterminationType = "ab", "c"
		b.Status, b.DeterminationType = "a", "bc"
		Expect(artifacts.ExportPairStateToken(a)).NotTo(Equal(artifacts.ExportPairStateToken(b)))
	})

	It("tells empty evidence from NULL evidence", func() {
		a, b := base(), base()
		a.Evidence, b.Evidence = []byte{}, nil
		Expect(artifacts.ExportPairStateToken(a)).NotTo(Equal(artifacts.ExportPairStateToken(b)))
	})
})

var _ = Describe("ValidateExpect", func() {
	DescribeTable("rejects a token that is not 16 lowercase hex characters, naming where to copy it from",
		func(token string) {
			err := artifacts.ValidateExpect(token)
			Expect(err).To(MatchError(ContainSubstring("not 16 lowercase hex characters")))
			Expect(err).To(MatchError(ContainSubstring("copy the EXPECT line")))
			Expect(err.Error()).NotTo(ContainSubstring("\x1b"))
			Expect(err.Error()).NotTo(ContainSubstring("\u202e"))
		},
		Entry("empty", ""),
		Entry("short", "0123"),
		Entry("long", "0123456789abcdef0"),
		Entry("uppercase", "0123456789ABCDEF"),
		Entry("non-hex", "0123456789abcdeg"),
		Entry("escape sequence", "\x1b[31m0123456789ab"),
		Entry("bidi override", "0123456789abcde\u202e"),
	)
})

var _ = Describe("PanelEvidence.Split", func() {
	It("counts same votes, and takes valid and total from the evidence", func() {
		yes, no := true, false
		ev := artifacts.PanelEvidence{Votes: []artifacts.VoteRecord{{Decision: &yes}, {Decision: &no}, {Decision: nil}, {Decision: &yes}},
			ValidVotes: 3, TotalVotes: 4}
		Expect(ev.Split()).To(Equal(artifacts.VoteSplit{Same: 2, Valid: 3, Total: 4}))
	})

	It("is all zero for empty evidence", func() {
		Expect(artifacts.PanelEvidence{}.Split()).To(Equal(artifacts.VoteSplit{}))
	})
})

var _ = Describe("PairChangedError", func() {
	It("matches ErrPairChanged and states the pair's current verdict", func() {
		err := error(&artifacts.PairChangedError{Ref: "0123456789abcdef", Status: artifacts.VerdictRejected,
			DeterminationType: "human", DeterminedBy: "bob", DecidedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)})
		Expect(errors.Is(err, artifacts.ErrPairChanged)).To(BeTrue())
		Expect(err.Error()).To(Equal(`decide human 0123456789abcdef: pair changed since it was viewed: it is now rejected by "bob" (human), decided 2026-10-02T09:00:00Z; view the pair again and decide from its current EXPECT token`))
	})

	It("says never for a pair no one has decided", func() {
		err := &artifacts.PairChangedError{Ref: "0123456789abcdef", Status: artifacts.VerdictPending, DeterminationType: "token_overlap"}
		Expect(err.Error()).To(ContainSubstring(`(token_overlap), decided never;`))
	})
})
