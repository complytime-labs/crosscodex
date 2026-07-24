package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	return fmt.Sprintf("error: %s", err.Error())
}
