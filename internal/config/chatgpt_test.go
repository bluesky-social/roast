package config

import (
	"strings"
	"testing"
)

func TestChatGPTProfile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("OPENAI_BASE_URL", "https://ambient.invalid/v1")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "gpt-test", MaxOutputTokens: 1024}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "" || role.BaseURL != "https://api.openai.com/v1" || role.Credential.Kind != CredentialChatGPTLogin {
		t.Fatalf("unexpected role: %#v", role)
	}
	if !strings.Contains(role.Credential.String(), "not verified") {
		t.Fatal(role.Credential.String())
	}
	profile := cfg.InferenceProfiles["finder"]
	if strings.Contains(profile.DescribeEndpoint(), "OPENAI") || strings.Contains(profile.DescribeCredential("finder"), "API_KEY") {
		t.Fatal("ChatGPT display offers API credentials")
	}
	role.Model = ""
	if ValidateRole("finder", role) == nil {
		t.Fatal("missing model accepted")
	}
}

func TestChatGPTRejectsProfileHTTPFields(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile InferenceProfile
	}{
		{"custom endpoint", InferenceProfile{BaseURL: "https://example.com"}},
		{"explicit default endpoint", InferenceProfile{BaseURL: "https://api.openai.com/v1"}},
		{"literal key", InferenceProfile{APIKey: "secret"}},
		{"key environment variable", InferenceProfile{APIKeyEnv: "MISSING_KEY"}},
		{"session header", InferenceProfile{SessionHeader: "X-Session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := tc.profile
			profile.Protocol, profile.Model = "chatgpt", "gpt-test"
			_, err := normalizeProfile("test", profile)
			if err == nil || !strings.Contains(err.Error(), "requires no endpoint") {
				t.Fatalf("normalizeProfile error = %v, want managed-login field rejection", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("validation disclosed API key")
			}
		})
	}
}

func TestChatGPTRejectsRoleOverrides(t *testing.T) {
	for _, variable := range []string{"ROAST_FINDER_API_KEY", "ROAST_FINDER_BASE_URL"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "private-value")
			cfg := Defaults()
			cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "gpt-test", MaxOutputTokens: 1024}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRole("finder", role); err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("unsafe validation: %v", err)
			}
		})
	}
}

func TestChatGPTCLIOverrides(t *testing.T) {
	chatgpt := "chatgpt"
	for _, kind := range []string{CredentialProviderEnv, CredentialProfileEnv, CredentialProfileKey, CredentialRoleEnv} {
		t.Run(kind, func(t *testing.T) {
			role := ResolvedRole{Protocol: "responses", BaseURL: DefaultBaseURL("responses"), Model: "gpt-test", APIKey: "secret", MaxOutputTokens: 1024, Credential: CredentialSource{Kind: kind}}
			role = OverrideEndpoint("finder", role, nil, &chatgpt)
			err := ValidateRole("finder", role)
			if kind == CredentialProviderEnv {
				if err != nil || role.APIKey != "" || role.Credential.Kind != CredentialChatGPTLogin {
					t.Fatalf("ambient override: %#v %v", role, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
	role := ResolvedRole{Protocol: "chatgpt", BaseURL: "https://api.openai.com/v1", Model: "gpt-test", MaxOutputTokens: 1024, Credential: CredentialSource{Kind: CredentialChatGPTLogin}}
	for _, endpoint := range []string{"https://example.com", "https://api.openai.com/v1"} {
		got := OverrideEndpoint("finder", role, &endpoint, nil)
		if ValidateRole("finder", got) == nil {
			t.Fatal("explicit endpoint accepted")
		}
	}
	t.Setenv("ROAST_FINDER_API_KEY", "secret")
	role = OverrideEndpoint("finder", role, nil, nil)
	if ValidateRole("finder", role) == nil {
		t.Fatal("role API key accepted")
	}
}

func TestChatGPTProtocolEnvironmentOverride(t *testing.T) {
	t.Setenv("ROAST_FINDER_PROTOCOL", "chatgpt")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("OPENAI_BASE_URL", "https://ambient.invalid/v1")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "responses", Model: "gpt-test", MaxOutputTokens: 1024}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRole("finder", role); err != nil {
		t.Fatal(err)
	}
	if role.APIKey != "" || role.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("ambient settings followed protocol override: %#v", role)
	}
	for _, profile := range []InferenceProfile{
		{Protocol: "responses", APIKey: "secret"},
		{Protocol: "responses", APIKeyEnv: "MISSING_KEY"},
		{Protocol: "responses", BaseURL: DefaultBaseURL("responses")},
		{Protocol: "responses", SessionHeader: "X-Session"},
	} {
		cfg.InferenceProfiles["finder"] = profile
		role, err := cfg.ResolveRole("finder")
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateRole("finder", role); err == nil {
			t.Fatalf("profile HTTP settings followed chatgpt override: %#v", profile)
		}
	}
}

func TestCLIProtocolCanRestoreOriginalProfileCredential(t *testing.T) {
	t.Setenv("ROAST_FINDER_PROTOCOL", "chatgpt")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "responses", Model: "test", APIKey: "original", MaxOutputTokens: 128}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"chat", "responses"} {
		if got := OverrideEndpoint("finder", role, nil, &protocol); ValidateRole("finder", got) != nil || got.APIKey != "original" {
			t.Fatalf("restoring the configured provider via %s lost its credential", protocol)
		}
	}
	protocol := "anthropic"
	if got := OverrideEndpoint("finder", role, nil, &protocol); got.APIKey == "original" {
		t.Fatal("profile credential followed the override to another provider")
	}
}

func TestProtocolFlagKeepsExplicitRoleEndpointPairedWithKey(t *testing.T) {
	for _, first := range []string{"chatgpt", "anthropic"} {
		t.Run(first, func(t *testing.T) {
			endpoint := DefaultBaseURL(first)
			t.Setenv("ROAST_FINDER_PROTOCOL", first)
			t.Setenv("ROAST_FINDER_BASE_URL", endpoint)
			t.Setenv("ROAST_FINDER_API_KEY", "role-key")
			cfg := Defaults()
			cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 128}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			protocol := "responses"
			if first == "chatgpt" {
				protocol = "anthropic"
			}
			got := OverrideEndpoint("finder", role, nil, &protocol)
			if err := ValidateRole("finder", got); err != nil || got.BaseURL != endpoint || got.APIKey != "role-key" {
				t.Fatalf("explicit role endpoint lost: %s, %v", got.BaseURL, err)
			}
		})
	}
}

func TestProtocolOverrideKeepsExplicitProfileEndpointWithRoleKey(t *testing.T) {
	t.Setenv("ROAST_FINDER_API_KEY", "role-key")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "responses", BaseURL: OpenAIBaseURL, Model: "test", MaxOutputTokens: 128}
	for _, source := range []string{"flag", "environment"} {
		t.Run(source, func(t *testing.T) {
			protocol := "anthropic"
			if source == "environment" {
				t.Setenv("ROAST_FINDER_PROTOCOL", protocol)
			}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			if source == "flag" {
				role = OverrideEndpoint("finder", role, nil, &protocol)
			}
			if role.BaseURL != OpenAIBaseURL || role.APIKey != "role-key" {
				t.Fatalf("role key moved to %s", role.BaseURL)
			}
		})
	}
}

func TestSwitchFromChatGPTUsesTheAPIKeyProvidersEndpoint(t *testing.T) {
	const gateway = "https://gateway.example/v1"
	t.Setenv("OPENAI_BASE_URL", gateway)
	t.Setenv("OPENAI_API_KEY", "gateway-key")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 128}
	for _, source := range []string{"flag", "environment"} {
		t.Run(source, func(t *testing.T) {
			protocol := "responses"
			if source == "environment" {
				t.Setenv("ROAST_FINDER_PROTOCOL", protocol)
			}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			if source == "flag" {
				role = OverrideEndpoint("finder", role, nil, &protocol)
			}
			if err := ValidateRole("finder", role); err != nil {
				t.Fatal(err)
			}
			if role.BaseURL != gateway || role.APIKey != "gateway-key" || role.Credential.Kind != CredentialProviderEnv {
				t.Fatalf("switching from ChatGPT should use the endpoint paired with OPENAI_API_KEY, got %s", role.BaseURL)
			}
		})
	}
}

func TestProtocolFlagRestoresExplicitProfileEndpointAndKey(t *testing.T) {
	t.Setenv("ROAST_FINDER_PROTOCOL", "chatgpt")
	t.Setenv("ROAST_FINDER_API_KEY", "")
	t.Setenv("ROAST_FINDER_BASE_URL", "")
	cfg := Defaults()
	cfg.InferenceProfiles["finder"] = InferenceProfile{
		Protocol: "anthropic", BaseURL: OpenAIBaseURL, APIKey: "profile-key", Model: "test", MaxOutputTokens: 128,
	}
	role, err := cfg.ResolveRole("finder")
	if err != nil {
		t.Fatal(err)
	}
	protocol := "anthropic"
	role = OverrideEndpoint("finder", role, nil, &protocol)
	if err := ValidateRole("finder", role); err != nil || role.BaseURL != OpenAIBaseURL || role.APIKey != "profile-key" {
		t.Fatalf("restoring the configured protocol changed its credential or endpoint: %s, %v", role.BaseURL, err)
	}
}

func TestNamingNewProviderEndpointDoesNotRestorePreviousKey(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ROAST_FINDER_PROTOCOL", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "destination-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic-gateway.example/v1")
	for _, source := range []string{"", "https://source-gateway.example/v1"} {
		cfg := Defaults()
		cfg.InferenceProfiles["finder"] = InferenceProfile{Protocol: "responses", APIKey: "source-key", BaseURL: source, Model: "test", MaxOutputTokens: 128}
		role, err := cfg.ResolveRole("finder")
		if err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []string{DefaultBaseURL("anthropic"), "https://anthropic-gateway.example/v1"} {
			got := OverrideEndpoint("finder", role, &endpoint, nil)
			if got.BaseURL != endpoint || got.APIKey != "destination-key" {
				t.Fatalf("switching from %q to %q selected the wrong endpoint or credential", source, endpoint)
			}
		}
		for _, endpoint := range []string{"https://api.anthropic.com:443/v1", "https://API.ANTHROPIC.COM./v1", "https://anthropic-gateway.example/custom"} {
			got := OverrideEndpoint("finder", role, &endpoint, nil)
			if got.BaseURL != endpoint || got.APIKey == "source-key" {
				t.Fatalf("source credential reached %q", endpoint)
			}
		}
	}
}
