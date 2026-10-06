package graphdb

import "time"

// TimeLayout is the fixed-width UTC layout drivers use to store temporal
// properties (valid_from, valid_to, analyzed_at). Fixed width makes lexical
// order equal chronological order, so a backend that compares the stored
// strings (Apache AGE's Cypher does) orders sub-second timestamps correctly.
// time.RFC3339Nano parses it. The layout is fixed-width only for years
// 0000-9999; times outside that range do not keep lexical == chronological
// order.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

// FormatTime renders t in UTC using TimeLayout.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeLayout)
}
