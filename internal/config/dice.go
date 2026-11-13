package config

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// ResolveDiceRoll selects from the configured pool without using fixed roles,
// their author routes, or role-specific environment overrides. Each selected
// profile supplies its own endpoint, protocol, model, and credential.
func (c Config) ResolveDiceRoll(verify bool) (ResolvedRole, ResolvedRole, error) {
	return c.resolveDiceRoll(verify, rand.IntN)
}

func (c Config) resolveDiceRoll(verify bool, intN func(int) int) (ResolvedRole, ResolvedRole, error) {
	pool := c.DiceRoll.Profiles
	if len(pool) == 0 {
		return ResolvedRole{}, ResolvedRole{}, fmt.Errorf("--dice-roll requires at least one profile in dice_roll.profiles")
	}
	seen := make(map[string]bool, len(pool))
	for _, name := range pool {
		if seen[name] {
			return ResolvedRole{}, ResolvedRole{}, fmt.Errorf("dice_roll.profiles contains duplicate profile %q", name)
		}
		seen[name] = true
		profile, ok := c.InferenceProfiles[name]
		if !ok {
			return ResolvedRole{}, ResolvedRole{}, fmt.Errorf("dice_roll.profiles references unknown inference profile %q", name)
		}
		if strings.TrimSpace(profile.Model) == "" {
			return ResolvedRole{}, ResolvedRole{}, fmt.Errorf("dice roll profile %q has no model", name)
		}
	}

	finderName := pool[intN(len(pool))]
	verifierName := ""
	if verify {
		finderModel := strings.TrimSpace(c.InferenceProfiles[finderName].Model)
		candidates := make([]string, 0, len(pool)-1)
		for _, name := range pool {
			if !strings.EqualFold(strings.TrimSpace(c.InferenceProfiles[name].Model), finderModel) {
				candidates = append(candidates, name)
			}
		}
		if len(candidates) == 0 {
			return ResolvedRole{}, ResolvedRole{}, fmt.Errorf("--dice-roll requires at least two distinct models when verification is enabled")
		}
		verifierName = candidates[intN(len(candidates))]
	}

	finder, err := c.resolveDiceProfile("finder", finderName)
	if err != nil {
		return ResolvedRole{}, ResolvedRole{}, err
	}
	if !verify {
		return finder, ResolvedRole{}, nil
	}
	verifier, err := c.resolveDiceProfile("verifier", verifierName)
	return finder, verifier, err
}

func (c Config) resolveDiceProfile(roleName, profileName string) (ResolvedRole, error) {
	profile := c.InferenceProfiles[profileName]
	role := ResolvedRole{
		Profile: profileName, BaseURL: profile.BaseURL, ExplicitEndpoint: profile.BaseURL != "",
		Model: profile.Model, Protocol: profile.Protocol, MaxOutputTokens: profile.MaxOutputTokens,
		SessionHeader: profile.SessionHeader,
	}
	if !role.ExplicitEndpoint {
		role.BaseURL = implicitEndpoint(role.Protocol)
	}
	role.APIKey, role.Credential = profileCredential(profile, role)
	if role.APIKey == "" {
		return ResolvedRole{}, fmt.Errorf("dice roll %s profile %q has no API key; set its api_key_env or api_key, or the provider API key for its endpoint", roleName, profileName)
	}
	if err := ValidateRole(roleName, role); err != nil {
		return ResolvedRole{}, fmt.Errorf("dice roll profile %q: %w", profileName, err)
	}
	return role, nil
}
