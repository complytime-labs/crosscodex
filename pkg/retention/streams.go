package retention

import (
	"sort"
	"time"
)

// StreamDrift reports whether one audit JetStream's live retention (MaxAge)
// matches the retention the configuration intends for it. It is an infra
// config-vs-live check, orthogonal to a data-retention scan, and is surfaced
// only in the admin GetRetentionStats response — never in retention.Report.
type StreamDrift struct {
	// Stream is the JetStream stream name (e.g. "AUDIT_LLM").
	Stream string
	// Configured is the retention the configuration intends for the stream.
	Configured time.Duration
	// Live is the retention currently applied on the server (0 when the
	// stream is absent from the live set).
	Live time.Duration
	// OK is true only when the stream exists live and its retention equals
	// the configured retention.
	OK bool
}

// VerifyStreamRetention compares the configured (intended) per-stream retention
// against the live per-stream retention read from the server. It returns one
// StreamDrift per stream present in configured, sorted by stream name for
// deterministic output. A stream missing from live is reported with Live=0 and
// OK=false (treated as not-found / drifted).
func VerifyStreamRetention(configured, live map[string]time.Duration) []StreamDrift {
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)

	drift := make([]StreamDrift, 0, len(names))
	for _, name := range names {
		want := configured[name]
		got, present := live[name]
		drift = append(drift, StreamDrift{
			Stream:     name,
			Configured: want,
			Live:       got,
			OK:         present && want == got,
		})
	}
	return drift
}
