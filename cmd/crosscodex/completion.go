package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newCompletionCmd() *cobra.Command {
	completion := &cobra.Command{
		Use:   "completion",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for bash, zsh, fish, and powershell.

Use the --install flag to write the completion script to the standard
autoload directory for your shell.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	completion.AddCommand(newBashCompletionCmd())
	completion.AddCommand(newZshCompletionCmd())
	completion.AddCommand(newFishCompletionCmd())
	completion.AddCommand(newPowerShellCompletionCmd())

	return completion
}

func newBashCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bash",
		Short: "Generate bash completion script",
		Long: `Generate a bash completion script.

To use in bash, add the completion script to ~/.bash_completion.d/crosscodex:
  crosscodex completion bash --install

Then source it in your ~/.bashrc:
  source ~/.bash_completion.d/crosscodex`,
		RunE: func(cmd *cobra.Command, args []string) error {
			install, _ := cmd.Flags().GetBool("install")
			root := cmd.Root()
			output := cmd.OutOrStdout()

			if install {
				home, _ := os.UserHomeDir()
				dir := filepath.Join(home, ".bash_completion.d")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", dir, err)
				}

				path := filepath.Join(dir, "crosscodex")
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
				if err != nil {
					return fmt.Errorf("create %s: %w", path, err)
				}
				defer file.Close()

				if err := root.GenBashCompletionV2(file, true); err != nil {
					return err
				}

				fmt.Fprintf(cmd.OutOrStderr(), "Bash completion installed to %s\n", path)
				fmt.Fprintf(cmd.OutOrStderr(), "Source it in your ~/.bashrc:\n  source %s\n", path)
				return nil
			}

			return root.GenBashCompletionV2(output, true)
		},
	}

	cmd.Flags().Bool("install", false, "install completion to standard location")
	return cmd
}

func newZshCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "zsh",
		Short: "Generate zsh completion script",
		Long: `Generate a zsh completion script.

To use in zsh, add the completion script to ~/.zfunc/_crosscodex:
  crosscodex completion zsh --install

Then add ~/.zfunc to your fpath in ~/.zshrc:
  fpath=(~/.zfunc $fpath)
  autoload -Uz compinit && compinit`,
		RunE: func(cmd *cobra.Command, args []string) error {
			install, _ := cmd.Flags().GetBool("install")
			root := cmd.Root()
			output := cmd.OutOrStdout()

			if install {
				home, _ := os.UserHomeDir()
				dir := filepath.Join(home, ".zfunc")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", dir, err)
				}

				path := filepath.Join(dir, "_crosscodex")
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
				if err != nil {
					return fmt.Errorf("create %s: %w", path, err)
				}
				defer file.Close()

				if err := root.GenZshCompletion(file); err != nil {
					return err
				}

				fmt.Fprintf(cmd.OutOrStderr(), "Zsh completion installed to %s\n", path)
				fmt.Fprintf(cmd.OutOrStderr(), "Add ~/.zfunc to your fpath in ~/.zshrc:\n  fpath=(~/.zfunc $fpath)\n")
				return nil
			}

			return root.GenZshCompletion(output)
		},
	}

	cmd.Flags().Bool("install", false, "install completion to standard location")
	return cmd
}

func newFishCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fish",
		Short: "Generate fish completion script",
		Long: `Generate a fish completion script.

To use in fish, install the completion script to ~/.config/fish/completions/:
  crosscodex completion fish --install

Then reload your shell or start a new session.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			install, _ := cmd.Flags().GetBool("install")
			root := cmd.Root()
			output := cmd.OutOrStdout()

			if install {
				configHome := os.Getenv("XDG_CONFIG_HOME")
				if configHome == "" {
					home, _ := os.UserHomeDir()
					configHome = filepath.Join(home, ".config")
				}

				dir := filepath.Join(configHome, "fish", "completions")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", dir, err)
				}

				path := filepath.Join(dir, "crosscodex.fish")
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
				if err != nil {
					return fmt.Errorf("create %s: %w", path, err)
				}
				defer file.Close()

				if err := root.GenFishCompletion(file, true); err != nil {
					return err
				}

				fmt.Fprintf(cmd.OutOrStderr(), "Fish completion installed to %s\n", path)
				return nil
			}

			return root.GenFishCompletion(output, true)
		},
	}

	cmd.Flags().Bool("install", false, "install completion to standard location")
	return cmd
}

func newPowerShellCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "powershell",
		Short: "Generate PowerShell completion script",
		Long: `Generate a PowerShell completion script.

To use in PowerShell, save the completion script and source it in your
PowerShell profile ($PROFILE).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			output := cmd.OutOrStdout()
			return root.GenPowerShellCompletionWithDesc(output)
		},
	}

	return cmd
}
