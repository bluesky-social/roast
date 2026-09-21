package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/snapshot"
)

func newReplayCommand(stdout, stderr io.Writer) *cobra.Command {
	var configPath, finderModel, verifierModel, into, format, maxSeverity string
	var keep, showDropped bool
	cmd := &cobra.Command{
		Use: "replay <run-id>", Short: "Re-run a review against the exact recorded tree and prompts",
		Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			meta, runDir, err := record.ReadMeta(args[0])
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if !meta.Snapshot.Captured {
				return &usageError{msg: fmt.Sprintf("run %s has no Git snapshot (%s)", meta.RunID, meta.Snapshot.Reason)}
			}
			if meta.Repo == "" {
				return &usageError{msg: fmt.Sprintf("run %s recorded no repository path", meta.RunID)}
			}
			finderSystem, err := replayArtifact(runDir, "finder_system", meta.Digests.System)
			if err != nil {
				return err
			}
			finderUser, err := replayArtifact(runDir, "finder_user", meta.Digests.User)
			if err != nil {
				return err
			}
			verifierSystem, err := replayArtifact(runDir, "verifier_system", meta.Roles["verifier"].PromptDigest)
			if err != nil {
				return err
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			finderRole, err := replayRole(meta.Roles["finder"], cfg, "finder", finderModel)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			verifierRole, err := replayRole(meta.Roles["verifier"], cfg, "verifier", verifierModel)
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			roles := map[string]config.ResolvedRole{"finder": finderRole, "verifier": verifierRole}

			dest := into
			var tempBase string
			if dest == "" {
				tempBase, err = os.MkdirTemp("", "roast-replay-")
				if err != nil {
					return fmt.Errorf("create replay directory: %w", err)
				}
				dest = filepath.Join(tempBase, "tree")
			}
			if _, err := snapshot.Restore(meta.Repo, meta.RunID, dest); err != nil {
				if tempBase != "" {
					_ = os.Remove(tempBase)
				}
				return fmt.Errorf("restore run %s: %w", meta.RunID, err)
			}
			if keep {
				fmt.Fprintf(stderr, "roast: keeping replay worktree at %s\n", dest)
			} else {
				defer func() {
					_ = snapshot.RemoveWorktree(meta.Repo, dest)
					if tempBase != "" {
						_ = os.Remove(tempBase)
					}
				}()
			}

			threshold := meta.Derived.MaxSeverity
			if cmd.Flags().Changed("max-severity") {
				threshold = maxSeverity
			}
			kind := meta.Scope.Kind
			if kind == "" {
				kind = "diff"
			}
			flags := &reviewFlags{
				configPath: configPath,
				base:       meta.Scope.BaseOID, uncommittedOnly: meta.Scope.UncommittedOnly,
				staged: meta.Scope.StagedOnly, task: meta.Derived.Task,
				format: format, maxSeverity: threshold,
				paths: append([]string(nil), meta.Scope.Paths...), includeVendored: meta.Scope.IncludeVendored,
				mode: meta.Derived.Mode, quick: meta.Derived.Quick, noVerify: !meta.Derived.DoVerify,
				interactive: meta.Derived.Interactive, maxTurns: meta.Derived.MaxTurns,
				deadline:       replayDuration(meta.Budgets, "deadline_duration"),
				requestTimeout: replayDurationDefault(meta.Budgets, "request_timeout", defaultRequestTimeout),
				maxTokensTotal: replayIntDefault(meta.Budgets, "max_total_tokens", defaultMaxTotalTokens),
				maxVerified:    replayIntDefault(meta.Budgets, "verifier_cap", defaultMaxVerified),
				maxAttempts:    replayIntDefault(meta.Budgets, "max_attempts", defaultMaxAttempts),
				showDropped:    showDropped, langs: append([]string(nil), meta.Derived.Langs...), scope: kind,
			}
			changedSet := map[string]bool{
				"deadline": true, "request-timeout": true, "max-tokens-total": true,
				"max-verified": true, "max-attempts": true, "max-turns": true,
			}
			return runReviewWithOptions(cmd.Context(), func(name string) bool { return changedSet[name] }, flags, stdout, stderr, reviewRunOptions{
				repo: dest, recordRepo: meta.Repo, replayOf: meta.RunID,
				roles: roles, exactExcludes: append([]string{}, meta.Scope.Excludes...), ceilingsResolved: true,
				finderSystem: finderSystem, finderUser: finderUser, verifierSystem: verifierSystem,
				expectedInputKind: meta.Digests.InputKind, expectedInput: meta.Digests.Input,
				argv: append([]string{"replay"}, args...),
			})
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&configPath, "config", "", "config file used only to resolve current API keys")
	flags.StringVar(&finderModel, "finder-model", "", "override the recorded finder model")
	flags.StringVar(&verifierModel, "verifier-model", "", "override the recorded verifier model")
	flags.StringVar(&into, "into", "", "restore into this directory instead of a temporary worktree")
	flags.BoolVar(&keep, "keep", false, "keep the restored worktree")
	flags.StringVar(&format, "format", "both", "json|markdown|both")
	flags.StringVar(&maxSeverity, "max-severity", "", "override the recorded severity gate")
	flags.BoolVar(&showDropped, "show-dropped", false, "render dropped and refuted findings")
	return cmd
}

func replayArtifact(runDir, name, digest string) (string, error) {
	content, err := record.ReadArtifact(runDir, name)
	if err != nil {
		return "", fmt.Errorf("read recorded %s: %w", name, err)
	}
	if digest != "" && record.Digest(content) != digest {
		return "", fmt.Errorf("recorded %s digest does not match metadata", name)
	}
	return content, nil
}

func replayRole(meta record.RoleMeta, cfg config.Config, roleName, modelOverride string) (config.ResolvedRole, error) {
	protocol := meta.WireProtocol()
	key := config.RoleScopedAPIKey(roleName)
	if meta.Profile != "" {
		current, err := cfg.ResolveProfile(roleName, meta.Profile)
		if err == nil && current.BaseURL == meta.BaseURL && current.Protocol == protocol {
			key = current.APIKey
		}
	} else if current, err := cfg.ResolveRole(roleName); err == nil && current.BaseURL == meta.BaseURL && current.Protocol == protocol {
		key = current.APIKey
	}
	role := config.ResolvedRole{
		Profile: meta.Profile, BaseURL: meta.BaseURL, APIKey: key, Model: meta.Model,
		Protocol: protocol, MaxOutputTokens: meta.MaxOutputTokens,
	}
	if modelOverride != "" {
		role.Model = modelOverride
	}
	if role.APIKey == "" {
		return config.ResolvedRole{}, fmt.Errorf("no current credential matches recorded %s profile %q (%s at %s)", roleName, meta.Profile, protocol, meta.BaseURL)
	}
	return role, nil
}

func replayDuration(values map[string]any, key string) time.Duration {
	if raw, ok := values[key].(string); ok {
		if duration, err := time.ParseDuration(raw); err == nil {
			return duration
		}
	}
	return -1
}

func replayDurationDefault(values map[string]any, key string, fallback time.Duration) time.Duration {
	value := replayDuration(values, key)
	if value < 0 {
		return fallback
	}
	return value
}

func replayIntDefault(values map[string]any, key string, fallback int) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return fallback
	}
}
