package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
	"github.com/bluesky-social/roast/internal/textsafe"
)

func newChatGPTCommand(stdout io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "chatgpt",
		Short: "Manage Roast's independent ChatGPT plan connection",
		Args:  noArgs,
		RunE:  showHelp,
	}
	root.AddCommand(
		newChatGPTSignInCommand(stdout),
		newChatGPTStatusCommand(stdout),
		newChatGPTSelectCommand(),
		newChatGPTSignOutCommand(stdout),
		newChatGPTModelsCommand(stdout),
	)
	root.SetFlagErrorFunc(flagUsageError)
	for _, command := range root.Commands() {
		command.Args = usageArgs(command.Args)
		command.SetFlagErrorFunc(flagUsageError)
	}
	return root
}

func newChatGPTSignInCommand(stdout io.Writer) *cobra.Command {
	var account string
	var enablePlan bool
	signin := &cobra.Command{
		Use:     "signin",
		Aliases: []string{"login"},
		Short:   "Continue with ChatGPT (authorize plan access in your browser)",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return err
			}
			if (enablePlan || cmd.Flags().Changed("account")) && !chatgpt.ValidAccountID(account) {
				return fmt.Errorf("specify an issued --account CLIENT_ID when using --enable-plan or --account")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			signIn := client.SignIn
			if enablePlan {
				signIn = client.EnablePlan
			}
			err = signIn(cmd.Context(), account, func(authURL string) error {
				_, err := fmt.Fprintf(stdout, "Continue with ChatGPT\nOpen this URL in a browser on this computer:\n%s\n", authURL)
				return err
			})
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, "ChatGPT account connected. Review usage and permissions at https://chatgpt.com/settings/usage"); err != nil {
				return fmt.Errorf("ChatGPT account connected, but could not write confirmation: %w", err)
			}
			return nil
		},
	}
	signin.Flags().BoolVar(&enablePlan, "enable-plan", false, "Explicitly request consent to enable plan usage for --account")
	signin.Flags().StringVar(&account, "account", "", "Reauthorize an existing issued client ID instead of registering a new account")
	return signin
}

func newChatGPTStatusCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List saved accounts and the active registration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			accounts, err := client.Status(ctx)
			if err != nil {
				return err
			}
			if len(accounts) == 0 {
				_, err := fmt.Fprintln(stdout, "No ChatGPT accounts. Run roast chatgpt signin.")
				return err
			}
			for _, account := range accounts {
				if _, err := fmt.Fprintf(stdout, "%s  %s  active=%t signed_in=%t plan_enabled=%t pending_registration=%t\n",
					textsafe.Line(account.ClientID), textsafe.Line(account.Email),
					account.Active, account.SignedIn, account.PlanEnabled, account.PendingRegistration); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newChatGPTSelectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "select CLIENT_ID",
		Short: "Select a saved ChatGPT registration",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return err
			}
			if !chatgpt.ValidAccountID(args[0]) {
				return fmt.Errorf("specify an issued CLIENT_ID")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			return client.Select(ctx, args[0])
		},
	}
}

func newChatGPTSignOutCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "signout [CLIENT_ID]",
		Short: "Clear this registration's local tokens and ask ChatGPT to revoke them",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return err
			}
			if len(args) == 1 && !chatgpt.ValidAccountID(args[0]) {
				return fmt.Errorf("specify an issued CLIENT_ID or omit it to sign out of the active account")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			if err := client.SignOut(ctx, id); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, "ChatGPT credentials cleared from this device. To confirm remote disconnection, check connected apps in ChatGPT Settings."); err != nil {
				return fmt.Errorf("cleared local ChatGPT credentials, but could not write confirmation: %w", err)
			}
			return nil
		},
	}
}

func newChatGPTModelsCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "List models available to the selected ChatGPT account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := chatgpt.New()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			models, err := client.Models(ctx)
			if err != nil {
				return err
			}
			for _, model := range models {
				if model.Visibility == "list" {
					if _, err := fmt.Fprintf(stdout, "%s\t%s\n", textsafe.Line(model.Slug), textsafe.Line(model.DisplayName)); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}
