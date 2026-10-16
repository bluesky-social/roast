package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/config"
)

func newConfigCommand(stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Initialize Roast configuration", Args: noArgs}
	cmd.AddCommand(newConfigInitCommand(stdout))
	return cmd
}

func newConfigInitCommand(stdout io.Writer) *cobra.Command {
	var sourceURL, destination string
	cmd := &cobra.Command{
		Use: "init", Short: "Install configuration from a trusted URL", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(sourceURL) == "" {
				_, _ = fmt.Fprint(stdout, "Configuration URL: ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				sourceURL = strings.TrimSpace(line)
				if sourceURL == "" {
					return &usageError{msg: "configuration URL is required"}
				}
			}
			legacy, _ := config.LegacyUserPath()
			if destination == "" {
				destination, _ = config.UserPath()
			}
			if err := config.Provision(cmd.Context(), sourceURL, destination, legacy); err != nil {
				return &usageError{msg: err.Error()}
			}
			_, _ = fmt.Fprintf(stdout, "roast: installed configuration at %s\n", destination)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceURL, "from", "", "HTTPS URL containing a complete Roast configuration")
	cmd.Flags().StringVar(&destination, "output", "", "config path (default: user config directory/roast/config.yaml)")
	return cmd
}

func maybeProvision(cmd *cobra.Command, explicitPath string, stdout io.Writer) error {
	if explicitPath != "" {
		return nil
	}
	path, err := config.UserPath()
	if err != nil {
		return err
	}
	legacy := ""
	if _, err := os.Stat(path); err == nil {
		needs, checkErr := config.NeedsProvision(path)
		if checkErr != nil {
			return checkErr
		}
		if !needs {
			return nil
		}
		legacy = path
	} else if !os.IsNotExist(err) {
		return err
	}
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return nil
	}
	stat, err := input.Stat()
	if err != nil || stat.Mode()&os.ModeCharDevice == 0 {
		return nil
	}
	return offerProvision(cmd, path, legacy, input, stdout)
}

func offerProvision(cmd *cobra.Command, path, legacy string, input io.Reader, stdout io.Writer) error {
	if legacy == "" {
		legacy, _ = config.LegacyUserPath()
	}
	if _, err := os.Stat(legacy); err == nil {
		_, _ = fmt.Fprintf(stdout, "Roast found a configuration to initialize or update at %s.\n", legacy)
		_, _ = fmt.Fprintln(stdout, "A trusted configuration URL can update matching Claude profiles to native Messages without replacing your other settings. Leave blank to keep intentional compatibility protocols.")
	} else {
		legacy = ""
		_, _ = fmt.Fprintln(stdout, "Roast is not configured.")
	}
	_, _ = fmt.Fprint(stdout, "Configuration URL (leave blank to configure manually): ")
	line, _ := bufio.NewReader(input).ReadString('\n')
	sourceURL := strings.TrimSpace(line)
	if sourceURL == "" {
		return config.AcknowledgeProtocolUpdate(path)
	}
	if err := config.Provision(cmd.Context(), sourceURL, path, legacy); err != nil {
		return &usageError{msg: err.Error()}
	}
	_, _ = fmt.Fprintf(stdout, "roast: installed configuration at %s\n", path)
	return nil
}
