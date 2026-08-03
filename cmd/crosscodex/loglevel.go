package main

import "log/slog"

// resolveLogLevel maps the CLI verbosity flags and the configured baseline level
// to an slog.Level. Precedence, highest first:
//
//   - --debug, or -v repeated twice or more → slog.LevelDebug
//   - -v once                               → slog.LevelInfo
//   - cfgLevel ("debug"/"info"/"warn"/"error") → the matching level
//   - anything else (empty or unrecognized) → slog.LevelWarn
//
// cfgLevel is config.Logging.Level, already validated by the config loader to be
// one of the four level names or empty.
func resolveLogLevel(verbose int, debug bool, cfgLevel string) slog.Level {
	switch {
	case debug || verbose >= 2:
		return slog.LevelDebug
	case verbose == 1:
		return slog.LevelInfo
	}

	switch cfgLevel {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
