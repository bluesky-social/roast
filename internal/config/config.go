// Package config loads user-controlled model and review configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	// CurrentVersion is the durable public configuration schema version.
	CurrentVersion = 1

	defaultOpenAIBaseURL    = "https://api.openai.com/v1"
	defaultAnthropicBaseURL = "https://api.anthropic.com/v1"
)

// DefaultBaseURL returns the public default endpoint for a wire protocol.
func DefaultBaseURL(protocol string) string {
	if protocol == "anthropic" {
		return defaultAnthropicBaseURL
	}
	return defaultOpenAIBaseURL
}

// InferenceProfile is one named model, endpoint, protocol, and credential
// source. Profiles live only in user-owned config, never repository config.
type InferenceProfile struct {
	BaseURL         string `yaml:"endpoint"`
	APIKey          string `yaml:"api_key"`
	APIKeyEnv       string `yaml:"api_key_env"`
	Model           string `yaml:"model"`
	Protocol        string `yaml:"protocol"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`
}

// Role selects an inference profile. The finder may route away from the
// author's model family through another named profile.
type Role struct {
	Profile string `yaml:"profile"`
	// Pin keeps this role selected even when it belongs to the same model
	// family as the author. Only the finder uses this setting.
	Pin bool `yaml:"pin"`
	// AuthorRoutes maps an exact author model or model family to a profile.
	AuthorRoutes map[string]string `yaml:"author_routes"`
}

// ResolvedRole is the complete, runtime-ready role. APIKey is secret and must
// never be copied into run metadata.
type ResolvedRole struct {
	Profile         string
	BaseURL         string
	APIKey          string
	Model           string
	Protocol        string
	MaxOutputTokens int
	Pin             bool
	AuthorRoutes    map[string]string
}

// Review contains local review defaults.
type Review struct {
	ConfidenceFloor      float64  `yaml:"confidence_floor"`
	QuickConfidenceFloor float64  `yaml:"quick_confidence_floor"`
	Excludes             []string `yaml:"excludes"`
}

// Prompts optionally replaces embedded prompt templates with user-owned files.
type Prompts struct {
	FinderCode  string `yaml:"finder_code"`
	FinderProse string `yaml:"finder_prose"`
	Verifier    string `yaml:"verifier"`
}

// Upload configures optional direct upload of deterministic run bundles to an
// S3-compatible object store. Credentials are named by environment variable;
// they are never accepted inline or copied into run metadata.
type Upload struct {
	Enabled           bool   `yaml:"enabled"`
	Endpoint          string `yaml:"endpoint"`
	Bucket            string `yaml:"bucket"`
	Prefix            string `yaml:"prefix"`
	Region            string `yaml:"region"`
	AccessKeyEnv      string `yaml:"access_key_env"`
	SecretKeyEnv      string `yaml:"secret_key_env"`
	SessionTokenEnv   string `yaml:"session_token_env"`
	PathStyle         bool   `yaml:"path_style"`
	AllowInsecureHTTP bool   `yaml:"allow_insecure_http"`
}

// ResolvedUpload is the runtime object-store configuration. Secret values are
// resolved only when an upload is attempted.
type ResolvedUpload struct {
	Endpoint, Bucket, Prefix, Region   string
	AccessKey, SecretKey, SessionToken string
	PathStyle                          bool
}

// Config is Roast's user configuration.
type Config struct {
	Version           int                         `yaml:"version"`
	InferenceProfiles map[string]InferenceProfile `yaml:"inference_profiles"`
	Roles             map[string]Role             `yaml:"roles"`
	Review            Review                      `yaml:"review"`
	Prompts           Prompts                     `yaml:"prompts"`
	Upload            Upload                      `yaml:"upload"`
}

// CredentialEnvNames returns every environment variable referenced as a
// profile credential source so artifact capture can always redact it.
func (c Config) CredentialEnvNames() []string {
	out := make([]string, 0)
	for _, profile := range c.InferenceProfiles {
		if profile.APIKeyEnv != "" {
			out = append(out, profile.APIKeyEnv)
		}
	}
	for _, name := range []string{c.Upload.AccessKeyEnv, c.Upload.SecretKeyEnv, c.Upload.SessionTokenEnv} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Defaults returns provider-neutral defaults. Model names are intentionally
// empty: users choose the models available at their endpoints.
func Defaults() Config {
	profile := InferenceProfile{
		BaseURL:         defaultOpenAIBaseURL,
		Protocol:        "responses",
		MaxOutputTokens: 32768,
	}
	return Config{
		Version: CurrentVersion,
		InferenceProfiles: map[string]InferenceProfile{
			"finder": profile, "verifier": profile,
		},
		Roles: map[string]Role{
			"finder": {Profile: "finder"}, "verifier": {Profile: "verifier"},
		},
		Review: Review{
			ConfidenceFloor:      0.6,
			QuickConfidenceFloor: 0.8,
		},
	}
}

// UserPath returns the default config file path.
func UserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "roast", "config.yaml"), nil
}

// Load reads and validates user configuration. An empty path selects UserPath;
// a missing default file is allowed, while a missing explicit file is an error.
func Load(path string) (Config, error) {
	explicit := path != ""
	if path == "" {
		var err error
		path, err = UserPath()
		if err != nil {
			return Config{}, err
		}
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return normalize(Defaults())
	}
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	cfg, err := decode(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return normalize(cfg)
}

func decode(data []byte) (Config, error) {
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(data, &header, false); err != nil {
		return Config{}, err
	}
	if header.Version != nil {
		if *header.Version != CurrentVersion {
			return Config{}, fmt.Errorf("unsupported config version %d (this build supports version %d)", *header.Version, CurrentVersion)
		}
		var cfg Config
		if err := decodeKnown(data, &cfg, true); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	shape, err := unversionedShape(data)
	if err != nil {
		return Config{}, err
	}
	switch shape {
	case shapeLegacyRoles:
		var legacy legacyConfig
		if err := decodeKnown(data, &legacy, true); err != nil {
			return Config{}, err
		}
		return migrateLegacy(legacy), nil
	case shapeDraftProfiles:
		var draft draftConfig
		if err := decodeKnown(data, &draft, true); err != nil {
			return Config{}, err
		}
		return migrateDraft(draft), nil
	default:
		var cfg Config
		if err := decodeKnown(data, &cfg, true); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}
}

func decodeKnown(data []byte, out any, knownFields bool) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(knownFields)
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config must contain exactly one YAML document")
		}
		return err
	}
	return nil
}

func normalize(cfg Config) (Config, error) {
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.Version != CurrentVersion {
		return Config{}, fmt.Errorf("unsupported config version %d (this build supports version %d)", cfg.Version, CurrentVersion)
	}

	defaults := Defaults()
	if cfg.InferenceProfiles == nil {
		cfg.InferenceProfiles = defaults.InferenceProfiles
	}
	if cfg.Roles == nil {
		cfg.Roles = defaults.Roles
	}
	for name, profile := range cfg.InferenceProfiles {
		normalized, err := normalizeProfile(name, profile)
		if err != nil {
			return Config{}, err
		}
		cfg.InferenceProfiles[name] = normalized
	}
	for _, name := range []string{"finder", "verifier"} {
		role, ok := cfg.Roles[name]
		if !ok || role.Profile == "" {
			role = defaults.Roles[name]
			cfg.Roles[name] = role
			if _, exists := cfg.InferenceProfiles[role.Profile]; !exists {
				cfg.InferenceProfiles[role.Profile] = defaults.InferenceProfiles[role.Profile]
			}
		}
		if _, ok := cfg.InferenceProfiles[role.Profile]; !ok {
			return Config{}, fmt.Errorf("roles.%s.profile references unknown inference profile %q", name, role.Profile)
		}
		normalizedRoutes := make(map[string]string, len(role.AuthorRoutes))
		for family, profileName := range role.AuthorRoutes {
			family = canonicalFamily(strings.ToLower(strings.TrimSpace(family)))
			if family == "" {
				return Config{}, fmt.Errorf("roles.%s.author_routes contains empty model family", name)
			}
			if _, ok := cfg.InferenceProfiles[profileName]; !ok {
				return Config{}, fmt.Errorf("roles.%s.author_routes.%s references unknown inference profile %q", name, family, profileName)
			}
			if _, duplicate := normalizedRoutes[family]; duplicate {
				return Config{}, fmt.Errorf("roles.%s.author_routes contains duplicate family %q", name, family)
			}
			normalizedRoutes[family] = profileName
		}
		role.AuthorRoutes = normalizedRoutes
		cfg.Roles[name] = role
	}
	if err := normalizeUpload(&cfg.Upload); err != nil {
		return Config{}, err
	}
	if cfg.Review.ConfidenceFloor == 0 {
		cfg.Review.ConfidenceFloor = defaults.Review.ConfidenceFloor
	}
	if cfg.Review.QuickConfidenceFloor == 0 {
		cfg.Review.QuickConfidenceFloor = defaults.Review.QuickConfidenceFloor
	}
	return cfg, nil
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func normalizeUpload(upload *Upload) error {
	if upload.AccessKeyEnv == "" {
		upload.AccessKeyEnv = "ROAST_UPLOAD_ACCESS_KEY"
	}
	if upload.SecretKeyEnv == "" {
		upload.SecretKeyEnv = "ROAST_UPLOAD_SECRET_KEY"
	}
	for field, name := range map[string]string{
		"access_key_env":    upload.AccessKeyEnv,
		"secret_key_env":    upload.SecretKeyEnv,
		"session_token_env": upload.SessionTokenEnv,
	} {
		if name != "" && !envNamePattern.MatchString(name) {
			return fmt.Errorf("upload.%s is not a valid environment variable name", field)
		}
	}
	upload.Endpoint = strings.TrimSpace(upload.Endpoint)
	upload.Bucket = strings.TrimSpace(upload.Bucket)
	upload.Prefix = strings.Trim(strings.TrimSpace(upload.Prefix), "/")
	if upload.Prefix == "" {
		upload.Prefix = "roast-runs"
	}
	for _, segment := range strings.Split(upload.Prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("upload.prefix must be a relative object-key prefix without empty, '.' or '..' segments")
		}
	}
	if upload.Endpoint != "" {
		u, err := url.Parse(upload.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("upload.endpoint must be an http(s) origin without credentials, a path, query, or fragment")
		}
		if u.Scheme == "http" && !loopbackHost(u.Hostname()) && !upload.AllowInsecureHTTP {
			return errors.New("upload.endpoint must use https outside loopback; set upload.allow_insecure_http only for a trusted local network")
		}
		u.Path = ""
		upload.Endpoint = strings.TrimSuffix(u.String(), "/")
	}
	if upload.Enabled && (upload.Endpoint == "" || upload.Bucket == "") {
		return errors.New("upload.endpoint and upload.bucket are required when upload.enabled is true")
	}
	return nil
}

func loopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ResolveUpload resolves object-store credentials from the configured
// environment variables. It intentionally does not consult AWS profiles,
// instance metadata, SSO, or any provider-specific credential chain.
func (c Config) ResolveUpload() (ResolvedUpload, error) {
	if c.Upload.Endpoint == "" || c.Upload.Bucket == "" {
		return ResolvedUpload{}, errors.New("upload endpoint and bucket are not configured")
	}
	access := os.Getenv(c.Upload.AccessKeyEnv)
	secret := os.Getenv(c.Upload.SecretKeyEnv)
	if access == "" || secret == "" {
		return ResolvedUpload{}, fmt.Errorf("upload credentials are not configured (set %s and %s)", c.Upload.AccessKeyEnv, c.Upload.SecretKeyEnv)
	}
	return ResolvedUpload{
		Endpoint: c.Upload.Endpoint, Bucket: c.Upload.Bucket,
		Prefix: c.Upload.Prefix, Region: c.Upload.Region,
		AccessKey: access, SecretKey: secret,
		SessionToken: os.Getenv(c.Upload.SessionTokenEnv), PathStyle: c.Upload.PathStyle,
	}, nil
}

func normalizeProfile(name string, profile InferenceProfile) (InferenceProfile, error) {
	if strings.TrimSpace(name) == "" {
		return InferenceProfile{}, errors.New("inference profile names cannot be empty")
	}
	if profile.Protocol == "" {
		profile.Protocol = "responses"
	}
	if profile.BaseURL == "" {
		profile.BaseURL = DefaultBaseURL(profile.Protocol)
	}
	if profile.MaxOutputTokens == 0 {
		profile.MaxOutputTokens = 32768
	}
	if profile.APIKey != "" && profile.APIKeyEnv != "" {
		return InferenceProfile{}, fmt.Errorf("inference_profiles.%s cannot set both api_key and api_key_env", name)
	}
	if profile.APIKeyEnv != "" && !envNamePattern.MatchString(profile.APIKeyEnv) {
		return InferenceProfile{}, fmt.Errorf("inference_profiles.%s.api_key_env is not a valid environment variable name", name)
	}
	if err := validateEndpoint("inference_profiles."+name, profile.BaseURL, profile.Protocol, profile.MaxOutputTokens); err != nil {
		return InferenceProfile{}, err
	}
	return profile, nil
}

// ResolveRole expands a role selection into the complete runtime role and
// applies role/provider environment overrides.
func (c Config) ResolveRole(name string) (ResolvedRole, error) {
	selection, ok := c.Roles[name]
	if !ok || selection.Profile == "" {
		return ResolvedRole{}, fmt.Errorf("role %q is not configured", name)
	}
	return c.resolveProfile(name, selection.Profile, selection)
}

// ResolveProfile resolves a named profile in the environment of a role. It is
// used by replay to recover a current credential for a recorded profile.
func (c Config) ResolveProfile(roleName, profileName string) (ResolvedRole, error) {
	return c.resolveProfile(roleName, profileName, Role{})
}

func (c Config) resolveProfile(roleName, profileName string, selection Role) (ResolvedRole, error) {
	profile, ok := c.InferenceProfiles[profileName]
	if !ok {
		return ResolvedRole{}, fmt.Errorf("role %s references unknown inference profile %q", roleName, profileName)
	}
	role := ResolvedRole{
		Profile: profileName, BaseURL: profile.BaseURL,
		Model: profile.Model, Protocol: profile.Protocol, MaxOutputTokens: profile.MaxOutputTokens,
		Pin: selection.Pin, AuthorRoutes: selection.AuthorRoutes,
	}
	applyRoleEnv(roleName, &role)
	role.APIKey = resolveAPIKey(roleName, role.Protocol, profile)
	return role, nil
}

func applyRoleEnv(name string, role *ResolvedRole) {
	prefix := "ROAST_" + strings.ToUpper(name) + "_"
	previousProtocol := role.Protocol
	if value := os.Getenv(prefix + "PROTOCOL"); value != "" {
		role.Protocol = value
		if role.BaseURL == DefaultBaseURL(previousProtocol) {
			role.BaseURL = DefaultBaseURL(role.Protocol)
		}
	}
	if role.Protocol == "anthropic" {
		if value := os.Getenv("ANTHROPIC_BASE_URL"); value != "" {
			role.BaseURL = value
		}
	} else if value := os.Getenv("OPENAI_BASE_URL"); value != "" {
		role.BaseURL = value
	}
	if value := os.Getenv(prefix + "BASE_URL"); value != "" {
		role.BaseURL = value
	}
	if value := os.Getenv(prefix + "MODEL"); value != "" {
		role.Model = value
	}
	if value := os.Getenv(prefix + "MAX_OUTPUT_TOKENS"); value != "" {
		if n, err := strconv.Atoi(value); err == nil {
			role.MaxOutputTokens = n
		}
	}
}

func resolveAPIKey(roleName, protocol string, profile InferenceProfile) string {
	if value := os.Getenv("ROAST_" + strings.ToUpper(roleName) + "_API_KEY"); value != "" {
		return value
	}
	if protocol == profile.Protocol {
		if profile.APIKeyEnv != "" {
			return os.Getenv(profile.APIKeyEnv)
		}
		if value := providerAPIKey(protocol); value != "" {
			return value
		}
		return profile.APIKey
	}
	return providerAPIKey(protocol)
}

func providerAPIKey(protocol string) string {
	if protocol == "anthropic" {
		return os.Getenv("ANTHROPIC_API_KEY")
	}
	return os.Getenv("OPENAI_API_KEY")
}

// RoleScopedAPIKey resolves only an explicitly role-scoped Roast credential.
// Replay uses this until it proves that current profile configuration matches
// the recorded endpoint and protocol.
func RoleScopedAPIKey(roleName string) string {
	return os.Getenv("ROAST_" + strings.ToUpper(roleName) + "_API_KEY")
}

// EnvironmentAPIKey resolves the environment credential for a role after its
// protocol is known. Role-specific Roast variables are provider-neutral;
// provider variables are selected only for their protocol.
func EnvironmentAPIKey(roleName, protocol string) string {
	if value := RoleScopedAPIKey(roleName); value != "" {
		return value
	}
	return providerAPIKey(protocol)
}

// AuthorModel returns the author model or family supplied by the invoking
// agent. A command-line override takes precedence in the CLI.
func AuthorModel() string {
	return os.Getenv("ROAST_AUTHOR_MODEL")
}

// InferFamily returns a provider-neutral model family when one can be
// determined from either a bare family or a common provider/model spelling.
func InferFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	if before, after, ok := strings.Cut(m, "/"); ok {
		switch before {
		case "anthropic", "openai", "google", "gemini", "mistral", "xai", "cohere", "deepseek":
			return canonicalFamily(before)
		}
		m = after
	}
	m = strings.TrimSuffix(strings.TrimSuffix(m, ":api"), "[1m]")
	switch {
	case m == "anthropic" || strings.HasPrefix(m, "claude-"):
		return "anthropic"
	case m == "openai" || strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "chatgpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	case m == "google" || m == "gemini" || strings.HasPrefix(m, "gemini-"):
		return "google"
	case m == "mistral" || strings.HasPrefix(m, "mistral-") || strings.HasPrefix(m, "codestral-"):
		return "mistral"
	case m == "xai" || strings.HasPrefix(m, "grok-"):
		return "xai"
	case m == "cohere" || strings.HasPrefix(m, "command-"):
		return "cohere"
	case m == "deepseek" || strings.HasPrefix(m, "deepseek-"):
		return "deepseek"
	default:
		return ""
	}
}

func canonicalFamily(family string) string {
	if family == "gemini" {
		return "google"
	}
	return family
}

// FlipResult describes author-aware finder selection.
type FlipResult struct {
	Role ResolvedRole
	Note string
	Warn string
}

// ResolveFinderFlip keeps finder generation independent from the author when
// the configured finder supplies a route for the author's model family.
func (c Config) ResolveFinderFlip(finder ResolvedRole, authorModel string) (FlipResult, error) {
	result := FlipResult{Role: finder}
	authorKey := strings.ToLower(strings.TrimSpace(authorModel))
	if authorKey == "" {
		return result, nil
	}
	authorFamily := InferFamily(authorModel)
	if finder.Pin {
		if authorFamily != "" && InferFamily(finder.Model) == authorFamily {
			result.Warn = fmt.Sprintf("author-model %s shares the finder family %s, but the finder is pinned; generation independence is reduced", authorModel, authorFamily)
		}
		return result, nil
	}
	targetName := finder.AuthorRoutes[authorKey]
	if targetName == "" && authorFamily != "" {
		targetName = finder.AuthorRoutes[authorFamily]
	}
	if targetName == "" {
		if authorFamily == "" {
			result.Warn = fmt.Sprintf("could not determine a model family for author-model %q and no exact author route is configured; keeping finder %s", authorModel, finder.Model)
		} else if InferFamily(finder.Model) == authorFamily {
			result.Warn = fmt.Sprintf("author-model %s shares the finder family %s and no author route is configured; generation independence is reduced", authorModel, authorFamily)
		}
		return result, nil
	}
	resolved, err := c.resolveProfile("finder", targetName, Role{})
	if err != nil {
		return result, err
	}
	targetFamily := InferFamily(resolved.Model)
	if (authorFamily != "" && (targetFamily == "" || targetFamily == authorFamily)) || strings.EqualFold(resolved.Model, authorModel) {
		result.Warn = fmt.Sprintf("author-model %s shares the finder family %s and profile %q uses non-independent model %q; keeping finder %s", authorModel, authorFamily, targetName, resolved.Model, finder.Model)
		return result, nil
	}
	result.Role = resolved
	if authorFamily == "" {
		result.Note = fmt.Sprintf("author-model %s matched an exact route; using finder profile %s (%s)", authorModel, targetName, resolved.Model)
	} else {
		result.Note = fmt.Sprintf("author-model %s maps to family %s; using finder profile %s (%s)", authorModel, authorFamily, targetName, resolved.Model)
	}
	return result, nil
}

// ValidateRole checks one fully-resolved role before any model call.
func ValidateRole(name string, role ResolvedRole) error {
	if strings.TrimSpace(role.Model) == "" {
		return fmt.Errorf("%s model is not configured (set inference_profiles.%s.model or ROAST_%s_MODEL)", name, role.Profile, strings.ToUpper(name))
	}
	if strings.TrimSpace(role.APIKey) == "" {
		return fmt.Errorf("%s API key is not configured (set inference_profiles.%s.api_key_env or ROAST_%s_API_KEY)", name, role.Profile, strings.ToUpper(name))
	}
	return validateEndpoint(name, role.BaseURL, role.Protocol, role.MaxOutputTokens)
}

func validateEndpoint(name, baseURL, protocol string, maxOutputTokens int) error {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s endpoint must be an http(s) URL without credentials, a query, or a fragment", name)
	}
	switch protocol {
	case "chat", "responses", "anthropic":
	default:
		return fmt.Errorf("%s protocol must be chat, responses, or anthropic", name)
	}
	if maxOutputTokens <= 0 {
		return fmt.Errorf("%s max_output_tokens must be positive", name)
	}
	return nil
}
