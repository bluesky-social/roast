package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/gate"
	"github.com/bluesky-social/roast/internal/scope"
)

func newGateCommand(stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "gate", Short: "Manage the native Git pre-commit review gate",
		Long: "The gate reviews exactly the staged commit. Findings at HIGH or above block; operational failures fail open with a warning.",
		Args: noArgs, SilenceUsage: true, SilenceErrors: true,
	}
	cmd.AddCommand(newGateStatusCommand(stdout), newGateEnableCommand(stdout), newGateDisableCommand(stdout))
	return cmd
}

func newGateStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Report gate status for this repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			status, err := gate.Inspect(repo)
			if err != nil {
				return err
			}
			decision := string(status.Decision)
			if decision == "" {
				decision = "none recorded"
			}
			fmt.Fprintf(stdout, "repo:     %s\ngate:     %s\ndecision: %s\n", status.Repo, status.Summary(), decision)
			return nil
		},
	}
}

func newGateEnableCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "enable", Short: "Install the pre-commit gate in this repository", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			action, status, err := gate.Install(repo)
			if err != nil {
				return err
			}
			if action == gate.ActionManual {
				fmt.Fprintf(stdout, "%s is not an appendable shell script; Roast left it untouched.\nAdd this line manually:\n\n    %s\n", status.HookPath, gate.ManualLine())
				return nil
			}
			if err := gate.RecordDecision(repo, gate.DecisionEnabled); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "gate enabled: %s\nbypass one commit with: ROAST_SKIP=1 git commit ...\n", status.HookPath)
			return nil
		},
	}
}

func newGateDisableCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use: "disable", Short: "Remove the Roast pre-commit gate", Args: noArgs,
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			repo, err := scope.GitRoot(".")
			if err != nil {
				return &usageError{msg: "not inside a Git repository"}
			}
			action, status, err := gate.Remove(repo)
			if err != nil {
				return err
			}
			if err := gate.RecordDecision(repo, gate.DecisionDeclined); err != nil {
				return err
			}
			if action == gate.ActionRemoved {
				fmt.Fprintf(stdout, "gate removed from %s\n", status.HookPath)
			} else {
				fmt.Fprintln(stdout, "no Roast gate was installed; recorded the decline")
			}
			return nil
		},
	}
}
