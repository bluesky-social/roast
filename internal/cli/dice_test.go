package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestProvisionSummaryIncludesDicePoolEndpoints(t *testing.T) {
	cfg := config.Config{
		InferenceProfiles: map[string]config.InferenceProfile{
			"pool": {Model: "m", BaseURL: "https://pool.example/v1", Protocol: "chat", APIKeyEnv: "POOL_KEY"},
		},
		DiceRoll: config.DiceRoll{Profiles: []string{"pool"}},
	}
	var output bytes.Buffer
	writeProvisionSummary(&output, &config.ProvisionPlan{Path: "config.yaml", Config: cfg})
	if !strings.Contains(output.String(), "dice:     profile pool, https://pool.example/v1, protocol chat, credential env POOL_KEY") {
		t.Fatalf("missing dice pool endpoint: %s", output.String())
	}
}

func TestDiceRollReviewUsesAndRecordsSelectedProfiles(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	t.Setenv("ROAST_FINDER_MODEL", "fixed-environment-model")
	t.Setenv("ROAST_VERIFIER_MODEL", "fixed-environment-model")
	t.Setenv("ROAST_FINDER_API_KEY", "incorrect-role-key")
	t.Setenv("ROAST_VERIFIER_API_KEY", "incorrect-role-key")
	t.Setenv("ROAST_FINDER_MAX_OUTPUT_TOKENS", "invalid")
	var mu sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Model != "one" && request.Model != "two" || r.Header.Get("Authorization") != "Bearer key-"+request.Model {
			http.Error(w, "unexpected model or credential", http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, request.Model)
		call := len(models)
		mu.Unlock()
		name := "findings"
		arguments := `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"unconditional panic","severity":"HIGH","confidence":0.95,"description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return"}]}`
		if call == 2 {
			name = "submit_verdict"
			arguments = `{"reasoning":"unconditional panic","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, name, arguments))
	}))
	defer server.Close()

	configBody := fmt.Sprintf(`version: 1
inference_profiles:
  fixed:
    model: fixed
  one:
    endpoint: %s/v1
    model: one
    protocol: chat
    api_key: key-one
  two:
    endpoint: %s/v1
    model: two
    protocol: chat
    api_key: key-two
roles:
  finder:
    profile: fixed
    author_routes:
      openai: fixed
  verifier:
    profile: fixed
dice_roll:
  profiles: [one, two]
`, server.URL, server.URL)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := initChangedRepo(t)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{
		"review", "--config", configPath, "--dice-roll", "--author-model", "openai", "--format", "json", "--max-attempts=1",
	}, &stdout, &stderr)
	if runErr != nil || code != ExitClean || !strings.Contains(stdout.String(), `"status":"confirmed"`) {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.RunID == "" {
		t.Fatalf("missing run id: %v: %s", err, stdout.String())
	}
	meta, _, err := record.ReadMeta(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) != 2 || models[0] == models[1] || meta.Roles["finder"].Model != models[0] || meta.Roles["verifier"].Model != models[1] {
		t.Fatalf("calls=%v metadata=%v", models, meta.Roles)
	}
	for _, role := range []string{"finder", "verifier"} {
		selected := meta.Roles[role]
		if selected.Profile != selected.Model || !strings.Contains(stderr.String(), "dice roll selected "+role+"="+selected.Model) {
			t.Fatalf("missing %s selection in logs or metadata: %#v: %s", role, selected, stderr.String())
		}
	}
}

func TestDiceRollCLIRejectsMissingPoolAndRoleOverrides(t *testing.T) {
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "requires at least one profile"},
		{[]string{"--finder-model", "fixed"}, "cannot be combined with --finder-model"},
		{[]string{"--verifier-base-url", "https://example.test"}, "cannot be combined with --verifier-base-url"},
	} {
		args := append([]string{"review", "--config", configPath, "--dice-roll"}, test.args...)
		code, err := ExecuteContext(t.Context(), args, io.Discard, io.Discard)
		if code != ExitUsage || err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args=%v code=%d err=%v, want %q", args, code, err, test.want)
		}
	}
}

func TestDiceRollQuickReviewNeedsOnlyOneProfile(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	var mu sync.Mutex
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		requested = append(requested, request.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"findings","arguments":"{\"assessment\":\"NO_FINDINGS\",\"findings\":[]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model))
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := fmt.Sprintf("version: 1\ninference_profiles:\n  only:\n    endpoint: %s/v1\n    model: only\n    protocol: chat\n    api_key: key\n  fixed:\n    endpoint: %s/v1\n    model: fixed\n    protocol: chat\n    api_key: key\nroles:\n  finder:\n    profile: fixed\n  verifier:\n    profile: fixed\ndice_roll:\n  profiles: [only]\n", server.URL, server.URL)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := initChangedRepo(t)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	var stdout, stderr bytes.Buffer
	code, runErr := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--dice-roll", "--quick", "--format", "json"}, &stdout, &stderr)
	if runErr != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	var result struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.RunID == "" {
		t.Fatalf("missing run id: %v: %s", err, stdout.String())
	}
	meta, _, err := record.ReadMeta(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Roles["finder"].Model != "only" || len(meta.Roles) != 1 {
		t.Fatalf("unexpected recorded roles: %v", meta.Roles)
	}
	stdout.Reset()
	stderr.Reset()
	code, runErr = ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--quick", "--format", "json"}, &stdout, &stderr)
	if runErr != nil || code != ExitClean {
		t.Fatalf("fixed review: code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.RunID == "" {
		t.Fatalf("missing fixed run id: %v: %s", err, stdout.String())
	}
	meta, _, err = record.ReadMeta(result.RunID)
	if err != nil || meta.Roles["finder"].Model != "fixed" {
		t.Fatalf("fixed role was not used: roles=%v err=%v", meta.Roles, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requested, ",") != "only,fixed" {
		t.Fatalf("requested models = %v, want dice pool then fixed role", requested)
	}
}
