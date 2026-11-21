//go:build darwin || linux

package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
	"github.com/bluesky-social/roast/internal/review"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestChatGPTCommands(t *testing.T) {
	for _, name := range []string{"signin", "login", "status", "select", "signout", "models"} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := newChatGPTCommand(&out)
			found, _, err := cmd.Find([]string{name})
			if err != nil || found == cmd {
				t.Fatalf("Find(%q) = %v, %v", name, found, err)
			}
			want := name
			if name == "login" {
				want = "signin"
			}
			if found.Name() != want {
				t.Errorf("command = %q, want %q", found.Name(), want)
			}
		})
	}
}

func TestChatGPTCommandRejectsInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"unknown", nil},
		{"status", []string{"--unexpected"}},
		{"signin", []string{"unexpected"}},
		{"signin", []string{"--enable-plan"}},
		{"signin", []string{"--account="}},
		{"signin", []string{"--account", "bad id"}},
		{"signin", []string{"--account", "dynamic_agent_client"}},
		{"select", []string{"bad id"}},
		{"signout", []string{"bad id"}},
		{"signout", []string{""}},
		{"status", []string{"unexpected"}},
		{"select", nil},
		{"select", []string{"first", "second"}},
		{"signout", []string{"first", "second"}},
		{"models", []string{"unexpected"}},
	} {
		t.Run(tc.command+"/"+strings.Join(tc.args, ","), func(t *testing.T) {
			args := append([]string{"chatgpt", tc.command}, tc.args...)
			code, err := ExecuteContext(t.Context(), args, io.Discard, io.Discard)
			if code != ExitUsage || err == nil {
				t.Fatalf("invalid arguments gave exit code %d and error %v, want usage error", code, err)
			}
		})
	}
}

func TestChatGPTEmptyStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	var out bytes.Buffer
	cmd := newChatGPTCommand(&out)
	cmd.SetArgs([]string{"status"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No ChatGPT accounts") {
		t.Fatalf("unexpected status: %s", out.String())
	}
}

func TestChatGPTModelsRequiresSignIn(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	code, err := ExecuteContext(t.Context(), []string{"chatgpt", "models", "--error-format", "json"}, io.Discard, io.Discard)
	if code != ExitAuth || !errors.Is(err, chatgpt.ErrAuth) {
		t.Fatalf("models without sign-in returned exit code %d, error %v", code, err)
	}
	var output bytes.Buffer
	WriteError(&output, code, err)
	var result struct {
		Category ErrorCategory `json:"error_category"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Category != ErrorAuth {
		t.Fatalf("expected a JSON authentication error, got %q", output.String())
	}
}

type accountRoundTripper func(*http.Request) (*http.Response, error)

func (f accountRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only synthetic, unexpired tokens in an isolated store are used. Replacing
// the whole transport makes any unexpected request fail instead of reaching a
// real auth or inference service. These tests must not run in parallel.
func mockAccountTransport(t *testing.T, transport accountRoundTripper) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func accountFixture(t *testing.T) (configPath, storePath string, selectAccount func(string) error) {
	t.Helper()
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	storePath = testutil.SeedChatGPTAccounts(t, "account-a", "account-b")
	selector, err := chatgpt.New()
	if err != nil {
		t.Fatal(err)
	}
	return configPath, storePath, func(account string) error { return selector.Select(t.Context(), account) }
}

func writeAccountConfig(t *testing.T, path, finder, verifier string) {
	t.Helper()
	var body strings.Builder
	body.WriteString("version: 1\ninference_profiles:\n")
	for i, role := range []string{"finder", "verifier"} {
		protocol := []string{finder, verifier}[i]
		fmt.Fprintf(&body, "  %s:\n    model: %s-model\n    protocol: %s\n    max_output_tokens: 1024\n", role, role, protocol)
		if protocol != "chatgpt" {
			body.WriteString("    endpoint: https://provider.invalid/v1\n    api_key: synthetic-api-key\n")
		}
	}
	body.WriteString("roles:\n  finder:\n    profile: finder\n  verifier:\n    profile: verifier\nupload:\n  enabled: false\n")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func accountToolResponse(model, tool, args string, direct bool) (*http.Response, error) {
	item := map[string]any{
		"id": "fc-test", "type": "function_call", "call_id": "call-test", "status": "completed",
		"name": tool, "arguments": args,
	}
	if direct {
		item["namespace"] = "roast"
	}
	w := httptest.NewRecorder()
	err := testutil.WriteInferenceStream(w, "responses", map[string]any{
		"id": "resp-test", "object": "response", "created_at": 1, "status": "completed", "model": model,
		"output": []any{item},
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	})
	return w.Result(), err
}

const unconditionalPanicFinding = `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is reachable","severity":"HIGH","confidence":0.95,"cwe":"CWE-248","description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return","frontend_category":""}]}`

func TestReviewProtocolFlagCanUseRoleAPISettings(t *testing.T) {
	configPath, _, _ := accountFixture(t)
	writeAccountConfig(t, configPath, "chatgpt", "responses")
	t.Chdir(initChangedRepo(t))
	t.Setenv("ROAST_FINDER_BASE_URL", "https://provider.invalid/v1")
	t.Setenv("ROAST_FINDER_API_KEY", "role-key")
	var calls int
	mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://provider.invalid/v1/responses" || r.Header.Get("Authorization") != "Bearer role-key" {
			return nil, errors.New("request did not use the explicitly selected API-key endpoint")
		}
		calls++
		return accountToolResponse("finder-model", "findings", `{"assessment":"NO_FINDINGS","findings":[]}`, false)
	})
	code, err := ExecuteContext(t.Context(), []string{"review", "--config", configPath, "--finder-protocol", "responses", "--quick", "--uncommitted-only", "--max-attempts=1"}, io.Discard, io.Discard)
	if err != nil || code != ExitClean || calls != 1 {
		t.Fatalf("protocol override: code=%d, calls=%d, err=%v", code, calls, err)
	}
}

func TestReviewPinsChatGPTAccountAcrossRoles(t *testing.T) {
	for _, protocols := range [][2]string{{"chatgpt", "chatgpt"}, {"responses", "chatgpt"}, {"chatgpt", "responses"}} {
		t.Run(strings.Join(protocols[:], "-"), func(t *testing.T) {
			configPath, _, selectAccount := accountFixture(t)
			writeAccountConfig(t, configPath, protocols[0], protocols[1])
			t.Chdir(initChangedRepo(t))
			var mu sync.Mutex
			var sent []string
			mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var request struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					return nil, err
				}
				mu.Lock()
				sent = append(sent, request.Model+":"+r.Header.Get("Authorization"))
				mu.Unlock()
				tool := "submit_verdict"
				args := `{"reasoning":"the panic is unconditional","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
				if request.Model == "finder-model" {
					// This models another process selecting B while finder A runs.
					if err := selectAccount("account-b"); err != nil {
						return nil, err
					}
					tool = "findings"
					args = unconditionalPanicFinding
				}
				return accountToolResponse(request.Model, tool, args, r.URL.Host == "api.openai.com")
			})
			for _, account := range []string{"account-a", "account-b"} {
				code, err := ExecuteContext(t.Context(), []string{
					"review", "--config", configPath, "--format", "json", "--uncommitted-only", "--max-attempts=1",
				}, io.Discard, io.Discard)
				if err != nil || code != ExitClean {
					t.Fatalf("operation starting with %s: code=%d err=%v", account, code, err)
				}
			}
			var want []string
			for _, account := range []string{"account-a", "account-b"} {
				for i, role := range []string{"finder", "verifier"} {
					token := "synthetic-api-key"
					if protocols[i] == "chatgpt" {
						token = "synthetic-" + account
					}
					want = append(want, role+"-model:Bearer "+token)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("requests = %v, want %v", sent, want)
			}
		})
	}
}

func TestDoctorPinsChatGPTAccountAcrossChecks(t *testing.T) {
	for _, finderProtocol := range []string{"chatgpt", "responses"} {
		t.Run(finderProtocol+"-finder", func(t *testing.T) {
			configPath, _, selectAccount := accountFixture(t)
			writeAccountConfig(t, configPath, finderProtocol, "chatgpt")
			var accounts []string
			var methods []string
			mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
				direct := r.URL.Host == "api.openai.com"
				if direct {
					accounts = append(accounts, r.Header.Get("Authorization"))
					methods = append(methods, r.Method)
				}
				// Both turns and the next role use the account selected at startup.
				if err := selectAccount("account-b"); err != nil {
					return nil, err
				}
				switch {
				case !direct && r.Method == http.MethodGet && r.URL.Path == "/v1/models":
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"finder-model"},{"slug":"verifier-model"}]}`))}, nil
				case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					var request struct{ Model string }
					if err := json.Unmarshal(body, &request); err != nil {
						return nil, err
					}
					tool, args := "doctor_lookup", `{}`
					if token := doctorTokenPattern.FindString(string(body)); token != "" {
						tool, args = "doctor_probe", fmt.Sprintf(`{"ok":true,"token":%q}`, token)
					}
					return accountToolResponse(request.Model, tool, args, direct)
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
			})
			for _, account := range []string{"account-a", "account-b"} {
				accounts, methods = nil, nil
				results := doctorChecks(t.Context(), configPath)
				for _, result := range results {
					if result.Status == doctorFail {
						t.Errorf("operation starting with %s: %#v", account, result)
					}
				}
				wantMethods := []string{"POST", "POST"}
				if finderProtocol == "chatgpt" {
					wantMethods = append(wantMethods, wantMethods...)
				}
				if !reflect.DeepEqual(methods, wantMethods) {
					t.Fatalf("direct requests = %v, want %v", methods, wantMethods)
				}
				for _, got := range accounts {
					if want := "Bearer synthetic-" + account; got != want {
						t.Errorf("account = %q, want %q", got, want)
					}
				}
			}
		})
	}
}

func TestDoctorEscapesChatGPTProviderErrors(t *testing.T) {
	configPath, _, _ := accountFixture(t)
	writeAccountConfig(t, configPath, "chatgpt", "chatgpt")
	t.Setenv("ROAST_FINDER_MODEL", "finder-model\n\x1b")
	mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/responses":
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("provider unavailable\n\x1b")),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	var out bytes.Buffer
	cmd := newDoctorCommand(&out)
	cmd.SetArgs([]string{"--config", configPath})
	if err := cmd.ExecuteContext(t.Context()); err == nil {
		t.Fatal("doctor succeeded despite provider failure")
	}
	if got := out.String(); !strings.Contains(got, `finder-model\n\x1b two-turn tool probe failed`) ||
		!strings.Contains(got, "--enable-plan") || strings.Contains(got, "provider unavailable") || strings.ContainsRune(got, '\x1b') {
		t.Fatalf("doctor output did not escape the provider diagnostic: %q", got)
	}
	reader, writer := io.Pipe()
	reader.Close()
	defer writer.Close()
	cmd = newDoctorCommand(writer)
	cmd.SetArgs([]string{"--config", configPath})
	if err := cmd.ExecuteContext(t.Context()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("doctor with closed output: %v", err)
	}
}

func TestDoctorSkipsLoginForInvalidChatGPTProfiles(t *testing.T) {
	configPath, _, _ := accountFixture(t)
	writeAccountConfig(t, configPath, "chatgpt", "chatgpt")
	t.Setenv("ROAST_FINDER_MODEL", " ")
	t.Setenv("ROAST_VERIFIER_MODEL", " ")
	mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("invalid profiles made a request to %s", r.URL)
		return nil, errors.New("unexpected request")
	})
	for _, result := range doctorChecks(t.Context(), configPath) {
		if strings.HasSuffix(result.Name, "-credential") {
			t.Errorf("invalid profile checked credentials: %#v", result)
		}
		if strings.HasSuffix(result.Name, "-profile") && result.Status != doctorFail {
			t.Errorf("invalid profile passed: %#v", result)
		}
	}
}

func TestReviewWithUnreadableChatGPTVerifierAccount(t *testing.T) {
	configPath, storePath, _ := accountFixture(t)
	writeAccountConfig(t, configPath, "responses", "chatgpt")
	if err := os.WriteFile(storePath, []byte("invalid account store"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(initChangedRepo(t))
	for _, tc := range []struct {
		name, response string
		exit           int
	}{
		{"no findings", `{"assessment":"NO_FINDINGS","findings":[]}`, ExitClean},
		{"finding retains its severity gate", unconditionalPanicFinding, ExitFindings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
				requests++
				if r.URL.Host != "provider.invalid" {
					return nil, fmt.Errorf("unexpected request to %s", r.URL)
				}
				return accountToolResponse("finder-model", "findings", tc.response, false)
			})
			var output bytes.Buffer
			code, err := ExecuteContext(t.Context(), []string{
				"review", "--config", configPath, "--format", "json", "--uncommitted-only", "--max-attempts=1", "--max-severity=HIGH",
			}, &output, io.Discard)
			if err != nil || code != tc.exit || requests != 1 {
				t.Fatalf("code=%d err=%v requests=%d", code, err, requests)
			}
			if tc.exit != ExitFindings {
				return
			}
			var result review.Result
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Findings) != 1 || result.Findings[0].Verdict == nil ||
				!result.Findings[0].Verdict.Undecided || !strings.Contains(result.Findings[0].Verdict.Reasoning, "initialize Sign in with ChatGPT") {
				t.Fatalf("expected the retained finding with a verifier authentication error: %s", output.String())
			}
		})
	}
}

func TestChatGPTBrowserSignInFeedsModelsAndReview(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA"} {
		t.Setenv(name, root)
	}
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	t.Setenv("ROAST_NO_HOOK", "1")
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	configPath := filepath.Join(root, "plan.yaml")
	writeAccountConfig(t, configPath, "chatgpt", "chatgpt")
	t.Chdir(initChangedRepo(t))

	key := testutil.NewRSAKey(t)
	authorization := make(chan url.Values, 1)
	var reviewRequests atomic.Int32
	asJSON := func(value any) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(recorder).Encode(value); err != nil {
			return nil, err
		}
		return recorder.Result(), nil
	}
	mockAccountTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case "https://auth.openai.com/api/accounts/oauth/token":
			var q url.Values
			select {
			case q = <-authorization:
			default:
				return nil, errors.New("unexpected second token exchange")
			}
			verifier := r.FormValue("code_verifier")
			challenge := sha256.Sum256([]byte(verifier))
			if r.Method != http.MethodPost || r.FormValue("grant_type") != "authorization_code" ||
				r.FormValue("code") != "test-code" || r.FormValue("client_id") != "test-account" ||
				r.FormValue("redirect_uri") != q.Get("redirect_uri") || verifier == "" ||
				q.Get("code_challenge_method") != "S256" || base64.RawURLEncoding.EncodeToString(challenge[:]) != q.Get("code_challenge") {
				return nil, errors.New("token exchange did not match the browser authorization")
			}
			now := time.Now()
			id := testutil.SignRS256(t, key, map[string]any{
				"iss": "https://auth.openai.com", "sub": "test-user", "aud": "test-account",
				"nonce": q.Get("nonce"), "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			})
			return asJSON(map[string]any{
				"access_token": "issued-access", "refresh_token": "issued-refresh", "id_token": id,
				"token_type": "Bearer", "expires_in": 3600, "scope": q.Get("scope"),
			})
		case "https://auth.openai.com/.well-known/jwks.json":
			return asJSON(testutil.RS256KeySet(key))
		}
		if r.Header.Get("Authorization") != "Bearer issued-access" {
			return nil, errors.New("command did not use the account from browser sign-in")
		}
		switch r.URL.String() {
		case "https://api.openai.com/v1/models":
			return asJSON(map[string]any{"models": []map[string]string{{"slug": "finder-model", "display_name": "Finder\nlabel\tvalue", "visibility": "list"}}})
		case "https://api.openai.com/v1/responses":
			reviewRequests.Add(1)
			return accountToolResponse("finder-model", "findings", `{"assessment":"NO_FINDINGS","findings":[]}`, true)
		default:
			return nil, fmt.Errorf("unexpected test provider request: %s", r.URL)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	signedIn := make(chan error, 1)
	go func() {
		_, err := ExecuteContext(ctx, []string{"chatgpt", "signin"}, writer, io.Discard)
		_ = writer.CloseWithError(err)
		signedIn <- err
	}()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "https://auth.openai.com/") {
			continue
		}
		start, err := url.Parse(scanner.Text())
		if err != nil {
			t.Fatal(err)
		}
		q := start.Query()
		if q.Get("client_id") != "dynamic_agent_client" || q.Get("ext_agent_host_id") == "" {
			t.Fatal("first sign-in did not request a new client for this installation")
		}
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || callback.Hostname() != "127.0.0.1" {
			t.Fatal("sign-in did not provide a local callback")
		}
		authorization <- q
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"test-code"}, "client_id": {"test-account"}}.Encode()
		browser := &http.Client{Transport: &http.Transport{}, Timeout: 3 * time.Second}
		resp, err := browser.Get(callback.String())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		browser.CloseIdleConnections()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("callback HTTP %d", resp.StatusCode)
		}
	}
	if err := <-signedIn; err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := ExecuteContext(ctx, []string{"chatgpt", "models"}, &out, io.Discard); err != nil || out.String() != "finder-model\tFinder\\nlabel\\tvalue\n" {
		t.Fatalf("model listing: %q, %v", out.String(), err)
	}
	closedReader, closedWriter := io.Pipe()
	closedReader.Close()
	defer closedWriter.Close()
	for _, command := range []string{"models", "status"} {
		if _, err := ExecuteContext(ctx, []string{"chatgpt", command}, closedWriter, io.Discard); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("%s with closed output: %v", command, err)
		}
	}
	out.Reset()
	code, err := ExecuteContext(ctx, []string{"review", "--config", configPath, "--uncommitted-only", "--max-attempts=1", "--format", "json"}, &out, io.Discard)
	if code != ExitClean || err != nil || reviewRequests.Load() != 1 {
		t.Fatalf("plan review: code=%d, requests=%d, error=%v", code, reviewRequests.Load(), err)
	}
}
