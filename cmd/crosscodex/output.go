package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

func emit(cmd *cobra.Command, human func(w io.Writer, color bool), jsonValue any) error {
	if jsonMode(cmd) {
		return writeJSON(cmd, jsonValue)
	}
	human(cmd.OutOrStdout(), useColor(cmd))
	return nil
}

func jsonMode(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("json")
	if !v && cmd.HasParent() {
		return jsonMode(cmd.Parent())
	}
	return v
}

func plainMode(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("plain")
	if !v && cmd.HasParent() {
		return plainMode(cmd.Parent())
	}
	return v
}

func useColor(cmd *cobra.Command) bool {
	if jsonMode(cmd) || plainMode(cmd) {
		return false
	}
	noColor, _ := cmd.Flags().GetBool("no-color")
	if noColor {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if v, ok := os.LookupEnv("CROSSCODEX_COLOR"); ok {
		return v == "1"
	}
	if f, ok := cmd.OutOrStdout().(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

func writeJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func formatCLIError(err error, jsonOut bool) string {
	if jsonOut {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(b)
	}
	return "error: " + renderError(err)
}

// renderError formats err for a terminal. When err is made of configuration
// problems (config.FieldErrors), it is one summary line (the context before
// the first problem, or "the configuration does not load" when there is
// none) and then each problem as "  - <message>" on its own
// line, without the sentinel text every problem's message ends with. Any
// other error is its message unchanged. Only the rendering changes: err
// itself, and JSON output, keep the full text.
func renderError(err error) string {
	msg := err.Error()
	fields := config.FieldErrors(err)
	if len(fields) == 0 || !config.FieldErrorsComplete(err) {
		return msg
	}
	summary := msg
	if i := strings.Index(msg, fields[0].Error()); i >= 0 {
		summary = msg[:i]
	}
	summary = trimSentinels(summary)
	if summary == "" {
		summary = "the configuration does not load"
	}
	var b strings.Builder
	b.WriteString(summary + ":")
	for _, fe := range fields {
		b.WriteString("\n  - " + trimSentinels(fe.Error()))
	}
	return b.String()
}

// trimSentinels removes the separators and sentinel texts ("invalid
// configuration", "failed to load configuration", ...) that wrapping leaves
// at the end of a message.
func trimSentinels(s string) string {
	suffixes := []string{config.ErrWrongType.Error(), config.ErrInvalidConfig.Error(), config.ErrLoadFailed.Error()}
	for {
		before := s
		s = strings.TrimRight(s, ": \n")
		for _, suffix := range suffixes {
			s = strings.TrimSuffix(s, suffix)
		}
		if s == before {
			return s
		}
	}
}
