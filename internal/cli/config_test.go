package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestConfigInitRejectsBlankInteractiveURL(t *testing.T) {
	var stdout bytes.Buffer
	cmd := newConfigInitCommand(&stdout)
	cmd.SetIn(strings.NewReader("   \n"))
	err := cmd.Execute()
	if err == nil || err.Error() != "configuration URL is required" {
		t.Fatalf("expected required URL error, got %v", err)
	}
}

func TestProtocolUpdateThroughConfigurationURLUsesNativeMessages(t *testing.T) {
	for _, entrypoint := range []string{"enrollment", "config init"} {
		t.Run(entrypoint, func(t *testing.T) {
			for _, name := range []string{"ROAST_FINDER_BASE_URL", "ROAST_FINDER_MODEL", "ROAST_FINDER_PROTOCOL", "ROAST_FINDER_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"} {
				t.Setenv(name, "")
			}
			var calls atomic.Int32
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "local-key" {
					t.Error("migrated client did not use native Messages with the preserved credential")
					http.Error(w, "wrong inference request", http.StatusBadRequest)
					return
				}
				var request struct {
					MaxTokens int  `json:"max_tokens"`
					Stream    bool `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.MaxTokens != 32768 || !request.Stream {
					t.Errorf("migrated native request lost its full budget or streaming: request=%+v error=%v", request, err)
					http.Error(w, "invalid native request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "anthropic", `{"id":"msg","type":"message","role":"assistant","model":"claude-test","content":[{"type":"tool_use","id":"call","name":"verdict","input":{"ok":true}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer models.Close()
			document := fmt.Sprintf(`version: 1
inference_profiles:
  finder:
    endpoint: %s/v1
    model: claude-test
    protocol: chat
    api_key: local-key
    max_output_tokens: 32768
  verifier:
    model: gpt-test
    protocol: responses
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
upload:
  enabled: false
`, models.URL)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			template := strings.Replace(document, "    api_key: local-key\n", "", 1)
			template = strings.Replace(template, "protocol: chat", "protocol: anthropic", 1)
			configuration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
					t.Error("template received local credentials")
				}
				_, _ = fmt.Fprint(w, template)
			}))
			defer configuration.Close()
			var output bytes.Buffer
			cmd := newConfigInitCommand(&output)
			cmd.SetContext(t.Context())
			if entrypoint == "enrollment" {
				if err := offerProvision(cmd, path, path, strings.NewReader(configuration.URL+"\n"), &output); err != nil {
					t.Fatal(err)
				}
			} else {
				cmd.SetArgs([]string{"--from", configuration.URL, "--output", path})
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			role, err := cfg.ResolveRole("finder")
			if err != nil {
				t.Fatal(err)
			}
			client, err := llm.New(t.Context(), llm.ClientConfig{
				BaseURL: role.BaseURL, Model: role.Model, APIKey: role.APIKey,
				Protocol: role.Protocol, MaxOutputTokens: role.MaxOutputTokens, MaxAttempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				OK bool `json:"ok"`
			}
			if err := client.CompleteStructured(t.Context(), "system", "user", "verdict", &result); err != nil || !result.OK || calls.Load() != 1 {
				t.Fatalf("native call failed: result=%v calls=%d error=%v", result, calls.Load(), err)
			}
			if needs, err := config.NeedsProvision(path); err != nil || needs {
				t.Fatalf("enrollment would fire again: needs=%v error=%v", needs, err)
			}
		})
	}
}

func TestProtocolUpdateDeclineAndNoninteractiveLeaveConfigAlone(t *testing.T) {
	for _, mode := range []string{"decline", "noninteractive", "explicit config"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
				t.Setenv(name, root)
			}
			path, err := config.UserPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			const raw = "version: 1\ninference_profiles:\n  finder:\n    endpoint: https://compat.example/v1\n    model: claude-test\n    protocol: chat\n"
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd := newConfigInitCommand(&output)
			cmd.SetIn(strings.NewReader("\n"))
			if mode == "decline" {
				err = offerProvision(cmd, path, path, cmd.InOrStdin(), &output)
			} else {
				explicit := ""
				if mode == "explicit config" {
					explicit = path
				}
				err = maybeProvision(cmd, explicit, &output)
				if output.Len() != 0 {
					t.Fatal("unexpected enrollment prompt")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != raw {
				t.Fatal("config changed without consent")
			}
			needs, err := config.NeedsProvision(path)
			if err != nil || needs != (mode != "decline") {
				t.Fatalf("decline/noninteractive acknowledgement: needs=%v error=%v", needs, err)
			}
		})
	}
}
