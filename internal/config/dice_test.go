package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiceRollSelectsCompleteDistinctProfiles(t *testing.T) {
	t.Setenv("FIRST_KEY", "first-key")
	t.Setenv("SECOND_KEY", "second-key")
	t.Setenv("ROAST_FINDER_MODEL", "fixed-finder")
	t.Setenv("ROAST_FINDER_BASE_URL", "https://wrong.example/v1")
	t.Setenv("ROAST_FINDER_API_KEY", "wrong-key")
	t.Setenv("ROAST_FINDER_PROTOCOL", "invalid")
	t.Setenv("ROAST_VERIFIER_MODEL", "fixed-verifier")
	t.Setenv("ROAST_VERIFIER_API_KEY", "wrong-key")
	cfg, err := Load(writeConfig(t, `version: 1
inference_profiles:
  first:
    model: first-model
    endpoint: https://first.example/v1
    protocol: chat
    api_key_env: FIRST_KEY
  second:
    model: second-model
    endpoint: https://second.example/v1
    protocol: anthropic
    api_key_env: SECOND_KEY
  same-model:
    model: FIRST-MODEL
    endpoint: https://third.example/v1
    api_key: third-key
roles:
  finder:
    profile: first
    author_routes:
      openai: second
  verifier:
    profile: second
dice_roll:
  profiles: [first, second, same-model]
`))
	if err != nil {
		t.Fatal(err)
	}
	for firstChoice := range 3 {
		t.Run(fmt.Sprint(firstChoice), func(t *testing.T) {
			calls := 0
			finder, verifier, err := cfg.resolveDiceRoll(true, func(n int) int {
				calls++
				if calls == 1 {
					return firstChoice
				}
				return n - 1
			})
			if err != nil {
				t.Fatal(err)
			}
			if finder.Profile != cfg.DiceRoll.Profiles[firstChoice] || strings.EqualFold(finder.Model, verifier.Model) {
				t.Fatalf("selected finder=%q/%q verifier=%q/%q", finder.Profile, finder.Model, verifier.Profile, verifier.Model)
			}
			for _, role := range []ResolvedRole{finder, verifier} {
				profile := cfg.InferenceProfiles[role.Profile]
				if role.Model != profile.Model || role.BaseURL != profile.BaseURL || role.Protocol != profile.Protocol || role.APIKey == "wrong-key" || role.APIKey == "" {
					t.Fatalf("role did not retain its profile: %#v", role)
				}
			}
		})
	}
}

func TestDiceRollPoolErrorsAndFinderOnly(t *testing.T) {
	base := Config{InferenceProfiles: map[string]InferenceProfile{
		"a":      {Model: "one", APIKey: "key", Protocol: "responses", MaxOutputTokens: 10},
		"b":      {Model: "ONE", APIKey: "key", Protocol: "responses", MaxOutputTokens: 10},
		"empty":  {APIKey: "key", Protocol: "responses", MaxOutputTokens: 10},
		"no-key": {Model: "two", BaseURL: "https://custom.example/v1", Protocol: "responses", MaxOutputTokens: 10},
	}}
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ROAST_FINDER_API_KEY", "ignored-key")
	for _, test := range []struct {
		name   string
		pool   []string
		verify bool
		want   string
	}{
		{"missing pool", nil, true, "requires at least one profile"},
		{"unknown profile", []string{"unknown"}, false, "unknown inference profile"},
		{"duplicate profile", []string{"a", "a"}, true, "duplicate profile"},
		{"missing model", []string{"empty"}, false, "has no model"},
		{"one model", []string{"a"}, true, "at least two distinct models"},
		{"same model", []string{"a", "b"}, true, "at least two distinct models"},
		{"missing profile credential", []string{"no-key"}, false, "has no API key"},
		{"finder only", []string{"a"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.DiceRoll.Profiles = test.pool
			finder, verifier, err := cfg.resolveDiceRoll(test.verify, func(int) int { return 0 })
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("got %v, want error containing %q", err, test.want)
				}
			} else if err != nil || finder.Profile != "a" || verifier.Profile != "" {
				t.Fatalf("finder=%#v verifier=%#v err=%v", finder, verifier, err)
			}
		})
	}
}

func TestDiceRollPoolRequiresBothFixedRoles(t *testing.T) {
	const base = "version: 1\ninference_profiles:\n  one:\n    model: one\ndice_roll:\n  profiles: [one]\n"
	for _, test := range []struct {
		name, roles, want string
	}{
		{"no roles", "", "roles.finder.profile"},
		{"missing verifier", "roles:\n  finder:\n    profile: one\n", "roles.verifier.profile"},
		{"missing finder", "roles:\n  verifier:\n    profile: one\n", "roles.finder.profile"},
		{"both roles", "roles:\n  finder:\n    profile: one\n  verifier:\n    profile: one\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, base+test.roles))
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}
