package retention_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/retention"
)

var _ = Describe("VerifyStreamRetention", func() {
	const (
		ninetyDays = 90 * 24 * time.Hour
		thirtyDays = 30 * 24 * time.Hour
	)

	It("reports OK for every stream when live matches configured", func() {
		configured := map[string]time.Duration{"AUDIT_LLM": ninetyDays, "AUDIT_EVENTS": thirtyDays}
		live := map[string]time.Duration{"AUDIT_LLM": ninetyDays, "AUDIT_EVENTS": thirtyDays}

		drift := retention.VerifyStreamRetention(configured, live)

		Expect(drift).To(HaveLen(2))
		for _, d := range drift {
			Expect(d.OK).To(BeTrue(), "stream %s should not have drifted", d.Stream)
			Expect(d.Configured).To(Equal(d.Live))
		}
	})

	It("flags only the stream whose live retention differs", func() {
		configured := map[string]time.Duration{"AUDIT_LLM": ninetyDays, "AUDIT_EVENTS": thirtyDays}
		live := map[string]time.Duration{"AUDIT_LLM": thirtyDays, "AUDIT_EVENTS": thirtyDays}

		drift := retention.VerifyStreamRetention(configured, live)

		Expect(drift).To(HaveLen(2))
		byStream := map[string]retention.StreamDrift{}
		for _, d := range drift {
			byStream[d.Stream] = d
		}
		Expect(byStream["AUDIT_LLM"].OK).To(BeFalse())
		Expect(byStream["AUDIT_LLM"].Configured).To(Equal(ninetyDays))
		Expect(byStream["AUDIT_LLM"].Live).To(Equal(thirtyDays))
		Expect(byStream["AUDIT_EVENTS"].OK).To(BeTrue())
	})

	It("treats a stream absent from live as drifted with Live=0", func() {
		configured := map[string]time.Duration{"AUDIT_LLM": ninetyDays, "AUDIT_EVENTS": thirtyDays}
		live := map[string]time.Duration{"AUDIT_LLM": ninetyDays}

		drift := retention.VerifyStreamRetention(configured, live)

		Expect(drift).To(HaveLen(2))
		byStream := map[string]retention.StreamDrift{}
		for _, d := range drift {
			byStream[d.Stream] = d
		}
		Expect(byStream["AUDIT_EVENTS"].OK).To(BeFalse())
		Expect(byStream["AUDIT_EVENTS"].Live).To(Equal(time.Duration(0)))
		Expect(byStream["AUDIT_EVENTS"].Configured).To(Equal(thirtyDays))
	})

	It("returns entries sorted by stream name for deterministic output", func() {
		configured := map[string]time.Duration{
			"AUDIT_LLM":       ninetyDays,
			"AUDIT_DECISIONS": 0,
			"AUDIT_EVENTS":    thirtyDays,
		}
		live := map[string]time.Duration{
			"AUDIT_LLM":       ninetyDays,
			"AUDIT_DECISIONS": 0,
			"AUDIT_EVENTS":    thirtyDays,
		}

		drift := retention.VerifyStreamRetention(configured, live)

		names := make([]string, 0, len(drift))
		for _, d := range drift {
			names = append(names, d.Stream)
		}
		Expect(names).To(Equal([]string{"AUDIT_DECISIONS", "AUDIT_EVENTS", "AUDIT_LLM"}))
	})
})
