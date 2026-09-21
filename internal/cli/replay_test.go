package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
)

func TestReplayResourceDefaultsForOlderRuns(t *testing.T) {
	var budgets map[string]any
	if got := replayDurationDefault(budgets, "request_timeout", 10*time.Minute); got != 10*time.Minute {
		t.Fatalf("request timeout = %s", got)
	}
	if got := replayIntDefault(budgets, "max_attempts", 4); got != 4 {
		t.Fatalf("max attempts = %d", got)
	}
	budgets = map[string]any{"request_timeout": "0s", "max_attempts": float64(1)}
	if got := replayDurationDefault(budgets, "request_timeout", 10*time.Minute); got != 0 {
		t.Fatalf("explicit request timeout = %s", got)
	}
	if got := replayIntDefault(budgets, "max_attempts", 4); got != 1 {
		t.Fatalf("explicit max attempts = %d", got)
	}
}

func TestReplayRoleDoesNotUseProviderCredentialForUnmatchedEndpoint(t *testing.T) {
	t.Setenv("ROAST_FINDER_API_KEY", "")
	t.Setenv("ROAST_API_KEY", "legacy-global-must-be-ignored")
	t.Setenv("OPENAI_API_KEY", "openai-provider-key")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-provider-key")

	for _, protocol := range []string{"responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config.Config{InferenceProfiles: map[string]config.InferenceProfile{
				"recorded": {
					BaseURL:         config.DefaultBaseURL(protocol),
					Model:           "current-model",
					Protocol:        protocol,
					MaxOutputTokens: 100,
				},
			}}
			meta := record.RoleMeta{
				Profile: "recorded", BaseURL: "https://gateway.example/v1",
				Model: "recorded-model", Protocol: protocol, MaxOutputTokens: 100,
			}

			_, err := replayRole(meta, cfg, "finder", "")
			if err == nil || !strings.Contains(err.Error(), "no current credential matches") {
				t.Fatalf("replayRole() error = %v, want unmatched credential error", err)
			}
		})
	}
}

func TestReplayRoleUsesProviderCredentialForMatchingEndpoint(t *testing.T) {
	t.Setenv("ROAST_FINDER_API_KEY", "")
	t.Setenv("ROAST_API_KEY", "legacy-global-must-be-ignored")
	t.Setenv("OPENAI_API_KEY", "openai-provider-key")

	baseURL := config.DefaultBaseURL("responses")
	cfg := config.Config{InferenceProfiles: map[string]config.InferenceProfile{
		"recorded": {
			BaseURL: baseURL, Model: "current-model", Protocol: "responses", MaxOutputTokens: 100,
		},
	}}
	meta := record.RoleMeta{
		Profile: "recorded", BaseURL: baseURL, Model: "recorded-model",
		Protocol: "responses", MaxOutputTokens: 100,
	}

	got, err := replayRole(meta, cfg, "finder", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "openai-provider-key" {
		t.Fatalf("API key = %q, want provider credential for matching endpoint", got.APIKey)
	}
}

func TestReplayRoleUsesExplicitRoastCredentialForUnmatchedEndpoint(t *testing.T) {
	t.Setenv("ROAST_FINDER_API_KEY", "replay-key")
	t.Setenv("ROAST_API_KEY", "legacy-global-must-be-ignored")
	t.Setenv("OPENAI_API_KEY", "openai-provider-key")

	meta := record.RoleMeta{
		Profile: "missing", BaseURL: "https://gateway.example/v1", Model: "recorded-model",
		Protocol: "responses", MaxOutputTokens: 100,
	}
	got, err := replayRole(meta, config.Config{}, "finder", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "replay-key" {
		t.Fatalf("API key = %q, want explicit role credential", got.APIKey)
	}
}
