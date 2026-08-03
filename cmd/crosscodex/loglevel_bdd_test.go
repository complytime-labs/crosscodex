package main

import (
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("resolveLogLevel", func() {
	DescribeTable("maps verbosity flags and config baseline to slog levels",
		func(verbose int, debug bool, cfgLevel string, expected slog.Level) {
			Expect(resolveLogLevel(verbose, debug, cfgLevel)).To(Equal(expected))
		},
		Entry("--debug wins over config", 0, true, "info", slog.LevelDebug),
		Entry("--debug wins over -v", 1, true, "", slog.LevelDebug),
		Entry("-vv means debug", 2, false, "", slog.LevelDebug),
		Entry("-vvv means debug", 3, false, "", slog.LevelDebug),
		Entry("-v means info", 1, false, "", slog.LevelInfo),
		Entry("flags override config baseline", 1, false, "error", slog.LevelInfo),
		Entry("config debug honored without flags", 0, false, "debug", slog.LevelDebug),
		Entry("config info honored without flags", 0, false, "info", slog.LevelInfo),
		Entry("config warn honored without flags", 0, false, "warn", slog.LevelWarn),
		Entry("config error honored without flags", 0, false, "error", slog.LevelError),
		Entry("empty config defaults to warn", 0, false, "", slog.LevelWarn),
		Entry("unrecognized config defaults to warn", 0, false, "bogus", slog.LevelWarn),
	)
})
