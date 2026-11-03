package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newConfigCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and modify CLI and project settings",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newConfigShowCmd(state))
	cmd.AddCommand(newConfigGetCmd(state))
	cmd.AddCommand(newConfigSetCmd(state))
	cmd.AddCommand(newConfigUnsetCmd(state))
	cmd.AddCommand(newConfigProfilesCmd(state))

	return cmd
}

func newConfigShowCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "show",
		Short:       "Show resolved configuration",
		Annotations: map[string]string{annotationNeedsResolvedConfig: "true"},
		Long:        `Show the resolved configuration from all layers (system, user, project, environment).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.loadErr != nil {
				return errConfigDoesNotLoad(state)
			}
			section, _ := cmd.Flags().GetString("section")

			var target any = state.fullCfg
			if section != "" {
				extracted, err := extractSection(state.fullCfg, section)
				if err != nil {
					return err
				}
				target = extracted
			}

			return emit(cmd, func(w io.Writer, color bool) {
				data, err := yaml.Marshal(target)
				if err != nil {
					fmt.Fprintf(w, "Error marshaling config: %v\n", err)
					return
				}
				if _, err = w.Write(data); err != nil {
					fmt.Fprintf(w, "Error writing config: %v\n", err)
				}
			}, target)
		},
	}

	cmd.Flags().String("section", "", "filter output to a specific top-level key (e.g., database, server, tls)")

	return cmd
}

func newConfigGetCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "get <key>",
		Annotations: map[string]string{annotationNeedsResolvedConfig: "true"},
		Short:       "Get a configuration value by key",
		Long: `Get a configuration value from the resolved config using dot-notation.

Returns the scalar value directly for leaf keys, or a YAML subtree for
struct/map keys. Use --json for machine-readable output.

Key format uses dot-notation matching yaml tags (e.g., database.dsn, cli.output).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if state.loadErr != nil {
				return errConfigDoesNotLoad(state)
			}
			key := args[0]

			val, err := getConfigValue(state.fullCfg, key)
			if err != nil {
				return err
			}

			if isScalar(val) {
				return emit(cmd, func(w io.Writer, color bool) {
					fmt.Fprintf(w, "%v\n", val)
				}, val)
			}

			return emit(cmd, func(w io.Writer, color bool) {
				data, err := yaml.Marshal(val)
				if err != nil {
					fmt.Fprintf(w, "Error marshaling config: %v\n", err)
					return
				}
				if _, err = w.Write(data); err != nil {
					fmt.Fprintf(w, "Error writing config: %v\n", err)
				}
			}, val)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a configuration key",
		find:  "crosscodex config show",
		usage: "crosscodex config get <key>",
		examples: []string{
			"crosscodex config get database.dsn",
			"crosscodex config get database",
			"crosscodex config get cli.output",
		},
	})

	return cmd
}

func getConfigValue(cfg *config.Config, key string) (any, error) {
	parts, err := config.SplitPath(key)
	if err != nil {
		return nil, err
	}
	current := reflect.ValueOf(cfg)

	for _, part := range parts {
		if current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return nil, fmt.Errorf("key %q not found in resolved configuration", key)
		}

		t := current.Type()
		found := false
		for fi := range t.NumField() {
			tag, _, _ := strings.Cut(t.Field(fi).Tag.Get("yaml"), ",")
			if tag == part {
				current = current.Field(fi)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("key %q not found in resolved configuration", key)
		}
	}

	return current.Interface(), nil
}

func isScalar(v any) bool {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return false
	default:
		return true
	}
}

// errConfigDoesNotLoad is what config show and get return when the
// configuration did not load: they have no resolved configuration to print.
// The root command prints no warning for them, so this is the one report.
func errConfigDoesNotLoad(state *cliState) error {
	return fmt.Errorf("the configuration does not load, so there is no resolved configuration to show. %s: %w",
		state.loadAdvice, state.loadErr)
}

func newConfigProfilesCmd(_ *cliState) *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "List available configuration profiles",
		Long:  `List configuration profiles in $XDG_CONFIG_HOME/crosscodex/profiles/.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			profilesDir := filepath.Join(xdgConfigHomeLocal(), "crosscodex", "profiles")

			entries, err := os.ReadDir(profilesDir)
			if err != nil {
				if os.IsNotExist(err) {
					return emit(cmd, func(w io.Writer, color bool) {
						fmt.Fprintf(w, "No profiles found in %s\n", profilesDir)
					}, []string{})
				}
				return fmt.Errorf("read profiles directory: %w", err)
			}

			var profiles []string
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
					profiles = append(profiles, strings.TrimSuffix(entry.Name(), ".yaml"))
				}
			}

			if len(profiles) == 0 {
				return emit(cmd, func(w io.Writer, color bool) {
					fmt.Fprintf(w, "No profiles found in %s\n", profilesDir)
				}, []string{})
			}

			return emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprintf(w, "Available profiles:\n")
				for _, p := range profiles {
					fmt.Fprintf(w, "  - %s\n", p)
				}
			}, profiles)
		},
	}
}

func extractSection(cfg *config.Config, section string) (any, error) {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == section {
			return v.Field(i).Interface(), nil
		}
	}
	var names []string
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Tag.Get("yaml"))
	}
	return nil, fmt.Errorf("unknown section %q (available: %s)", section, strings.Join(names, ", "))
}

func xdgConfigHomeLocal() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}
