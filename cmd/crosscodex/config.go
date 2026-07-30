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
	cmd.AddCommand(newConfigProfilesCmd(state))

	return cmd
}

func newConfigShowCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show resolved configuration",
		Long:  `Show the resolved configuration from all layers (system, user, project, environment).`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
		Use:   "get <key>",
		Short: "Get a configuration value by key",
		Long: `Get a configuration value from the resolved config using dot-notation.

Returns the scalar value directly for leaf keys, or a YAML subtree for
struct/map keys. Use --json for machine-readable output.

Key format uses dot-notation matching yaml tags (e.g., database.dsn, cli.output).`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	parts := strings.Split(key, ".")
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

func newConfigSetCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set a configuration value",
		Long: `Set a configuration value in the user config file.

Key format uses dot-notation (e.g., cli.output, cli.endpoint).
The value is written to $XDG_CONFIG_HOME/crosscodex/config.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return fmt.Errorf("requires both key and value")
			}

			key := args[0]
			value := args[1]

			if err := setConfigValue(key, value); err != nil {
				return fmt.Errorf("set config: %w", err)
			}

			return emit(cmd, func(w io.Writer, color bool) {
				fmt.Fprintf(w, "Set %s = %s\n", key, value)
			}, map[string]string{"key": key, "value": value})
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(2), argGuide{
		noun:  "a key and value",
		find:  "config set",
		usage: "crosscodex config set <key> <value>",
		examples: []string{
			"crosscodex config set cli.output json",
			"crosscodex config set cli.endpoint localhost:50051",
		},
	})

	return cmd
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

func setConfigValue(key, value string) error {
	userConfigPath := filepath.Join(xdgConfigHomeLocal(), "crosscodex", "config.yaml")
	userConfigDir := filepath.Dir(userConfigPath)

	if err := os.MkdirAll(userConfigDir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	var root yaml.Node
	data, err := os.ReadFile(userConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read user config: %w", err)
		}
		root = yaml.Node{Kind: yaml.DocumentNode}
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	} else {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse user config: %w", err)
		}
	}

	if len(root.Content) == 0 {
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}

	parts := strings.Split(key, ".")
	if err := setYAMLPath(root.Content[0], parts, value); err != nil {
		return err
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(userConfigPath, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

func setYAMLPath(node *yaml.Node, path []string, value string) error {
	if len(path) == 0 {
		return fmt.Errorf("empty path")
	}

	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected mapping node")
	}

	key := path[0]
	remaining := path[1:]

	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			if len(remaining) == 0 {
				node.Content[i+1].Value = value
				return nil
			}
			if node.Content[i+1].Kind != yaml.MappingNode {
				node.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode}
			}
			return setYAMLPath(node.Content[i+1], remaining, value)
		}
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	if len(remaining) == 0 {
		valueNode := &yaml.Node{Kind: yaml.ScalarNode, Value: value}
		node.Content = append(node.Content, keyNode, valueNode)
		return nil
	}

	valueNode := &yaml.Node{Kind: yaml.MappingNode}
	node.Content = append(node.Content, keyNode, valueNode)
	return setYAMLPath(valueNode, remaining, value)
}

func xdgConfigHomeLocal() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}
