package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const maxProvisionBytes = 1 << 20

// LegacyUserPath returns the XDG-style path used by the pre-public Roast.
func LegacyUserPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); dir != "" {
		return filepath.Join(dir, "roast", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "roast", "config.yaml"), nil
}

// NeedsProvision reports whether an organization template could initialize a
// legacy config or authorize a native-protocol update. A Claude model served
// through a compatibility protocol is only a candidate, not necessarily wrong.
func NeedsProvision(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil {
		return false, err
	}
	if header.Version == nil {
		return true, nil
	}
	cfg, err := decode(raw)
	if err != nil {
		return false, err
	}
	if len(compatibilityProfiles(cfg)) == 0 {
		return false, nil
	}
	acknowledged, err := os.ReadFile(path + ".protocol-update-ack")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return string(acknowledged) != fmt.Sprintf("%x\n", sha256.Sum256(raw)), nil
}

// AcknowledgeProtocolUpdate records a choice for these exact config bytes,
// without storing credentials or modifying the config. A later config edit
// invalidates the choice; explicit config init never consults this marker.
func AcknowledgeProtocolUpdate(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil || header.Version == nil {
		return err
	}
	cfg, err := decode(raw)
	if err != nil || len(compatibilityProfiles(cfg)) == 0 {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".protocol-update-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := fmt.Fprintf(file, "%x\n", sha256.Sum256(raw)); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path+".protocol-update-ack")
}

func compatibilityProfiles(cfg Config) []string {
	var names []string
	for name, profile := range cfg.InferenceProfiles {
		if InferFamily(profile.Model) == "anthropic" && (profile.Protocol == "" || profile.Protocol == "chat" || profile.Protocol == "responses") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Provision downloads a complete versioned configuration, optionally carries
// forward the credential from a legacy gateway config, and atomically installs
// it at path. The remote document may contain endpoint and model policy but no
// inline secrets.
func Provision(ctx context.Context, sourceURL, path, legacyPath string) error {
	if path == "" {
		var err error
		path, err = UserPath()
		if err != nil {
			return err
		}
	}
	raw, cfg, err := fetchProvision(ctx, sourceURL)
	if err != nil {
		return err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		var header struct {
			Version *int `yaml:"version"`
		}
		if err := decodeKnown(existing, &header, false); err != nil {
			return err
		}
		if header.Version != nil {
			updated, err := updateProtocols(existing, cfg)
			if err != nil {
				return err
			}
			if err := writeAtomic(path, updated, ".pre-protocol-update"); err != nil {
				return err
			}
			return AcknowledgeProtocolUpdate(path)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if legacyPath != "" {
		legacyRaw, readErr := os.ReadFile(legacyPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read legacy config: %w", readErr)
		}
		if readErr == nil {
			var legacy legacyConfig
			if err := decodeKnown(legacyRaw, &legacy, true); err != nil {
				return fmt.Errorf("parse legacy config: %w", err)
			}
			if legacy.Gateway.APIKey != "" && cfg.Provisioning != nil && cfg.Provisioning.ImportLegacyCredential {
				for name, profile := range cfg.InferenceProfiles {
					if profile.APIKey == "" && profile.APIKeyEnv == "" {
						profile.APIKey = legacy.Gateway.APIKey
						cfg.InferenceProfiles[name] = profile
					}
				}
				if cfg.Upload.Backend == "presigned" && cfg.Upload.Token == "" && cfg.Upload.TokenEnv == "" {
					cfg.Upload.Token = legacy.Gateway.APIKey
				}
				cfg.Provisioning = nil
				raw, err = yaml.Marshal(cfg)
				if err != nil {
					return fmt.Errorf("encode migrated config: %w", err)
				}
			}
		}
	}
	backupSuffix := ""
	if legacyPath != "" && filepath.Clean(path) == filepath.Clean(legacyPath) {
		backupSuffix = ".pre-v1"
	}
	return writeAtomic(path, raw, backupSuffix)
}

// updateProtocols uses an explicitly supplied template as the authority for
// endpoint support. It never infers gateway capabilities from model names alone
// and never replaces a versioned config with the template's unrelated settings.
func updateProtocols(raw []byte, template Config) ([]byte, error) {
	cfg, err := decode(raw)
	if err != nil {
		return nil, err
	}
	var updates []string
	for _, name := range compatibilityProfiles(cfg) {
		profile := cfg.InferenceProfiles[name]
		for _, target := range template.InferenceProfiles {
			if profile.BaseURL != "" && strings.TrimRight(profile.BaseURL, "/") == strings.TrimRight(target.BaseURL, "/") &&
				InferFamily(target.Model) == "anthropic" && target.Protocol == "anthropic" {
				updates = append(updates, name)
				profile.Protocol = "anthropic"
				cfg.InferenceProfiles[name] = profile
				break
			}
		}
	}
	if len(updates) == 0 {
		return nil, errors.New("config already exists; the template authorizes no matching Claude protocol updates (use --output for a new config)")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	profiles := yamlMappingValue(document.Content[0], "inference_profiles")
	for _, name := range updates {
		profile := yamlMappingValue(profiles, name)
		if profile == nil || profile.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("profile %q uses a YAML alias; update its protocol manually", name)
		}
		if node := yamlMappingValue(profile, "protocol"); node != nil {
			node.Kind, node.Tag, node.Value = yaml.ScalarNode, "!!str", "anthropic"
			node.Alias, node.Content = nil, nil
		} else {
			profile.Content = append(profile.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "protocol"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "anthropic"})
		}
	}
	updated, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	actual, err := decode(updated)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(actual, cfg) {
		return nil, errors.New("protocol update would change other settings through YAML aliases; update protocols manually")
	}
	if _, err := normalize(actual); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(updated), nil
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func fetchProvision(ctx context.Context, sourceURL string) ([]byte, Config, error) {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) || u.User != nil || u.Fragment != "" {
		return nil, Config{}, errors.New("configuration URL must be HTTPS, or HTTP on loopback, without credentials or a fragment")
	}
	originScheme, originHost := u.Scheme, u.Host
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("configuration URL redirected too many times")
			}
			if next.URL.Scheme != originScheme || next.URL.Host != originHost || next.URL.User != nil || next.URL.Fragment != "" {
				return errors.New("configuration URL redirected to another origin")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Config{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Config{}, fmt.Errorf("fetch configuration: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Config{}, fmt.Errorf("fetch configuration: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProvisionBytes+1))
	if err != nil {
		return nil, Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(raw) > maxProvisionBytes {
		return nil, Config{}, fmt.Errorf("configuration exceeds %d bytes", maxProvisionBytes)
	}
	cfg, err := ValidateProvisionDocument(raw)
	if err != nil {
		return nil, Config{}, err
	}
	return bytes.TrimSpace(raw), cfg, nil
}

// ValidateProvisionDocument validates an organization-provided configuration
// template. Templates may reference credential environment variables, but may
// not contain secret values.
func ValidateProvisionDocument(raw []byte) (Config, error) {
	var header struct {
		Version *int `yaml:"version"`
	}
	if err := decodeKnown(raw, &header, false); err != nil {
		return Config{}, fmt.Errorf("parse downloaded configuration: %w", err)
	}
	if header.Version == nil {
		return Config{}, errors.New("downloaded configuration must declare a version")
	}
	cfg, err := decode(raw)
	if err != nil {
		return Config{}, fmt.Errorf("parse downloaded configuration: %w", err)
	}
	cfg, err = normalize(cfg)
	if err != nil {
		return Config{}, fmt.Errorf("validate downloaded configuration: %w", err)
	}
	for name, profile := range cfg.InferenceProfiles {
		if profile.APIKey != "" {
			return Config{}, fmt.Errorf("downloaded configuration must not contain inference_profiles.%s.api_key", name)
		}
	}
	if cfg.Upload.Token != "" {
		return Config{}, errors.New("downloaded configuration must not contain upload.token")
	}
	return cfg, nil
}

func writeAtomic(path string, raw []byte, backupSuffix string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if backupSuffix == "" {
			return fmt.Errorf("config already exists at %s", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular config at %s", path)
		}
		backup := path + backupSuffix
		if err := ensureLegacyBackup(path, backup, 0o600); err != nil {
			return fmt.Errorf("back up config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}

func ensureLegacyBackup(source, destination string, mode os.FileMode) error {
	sourceRaw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("existing backup is not a regular file: %s", destination)
		}
		backupRaw, readErr := os.ReadFile(destination)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(sourceRaw, backupRaw) {
			return fmt.Errorf("existing backup does not match legacy config: %s", destination)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return copyExclusive(destination, sourceRaw, mode, persistExclusiveCopy)
}

func copyExclusive(destination string, raw []byte, mode os.FileMode, persist func(*os.File, []byte) error) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = os.Remove(destination)
		}
	}()
	if err := persist(file, raw); err != nil {
		return err
	}
	complete = true
	return nil
}

func persistExclusiveCopy(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}
