package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/bluesky-social/roast/internal/testutil"
)

type planRoundTripper func(*http.Request) (*http.Response, error)

func (f planRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func planStream(t *testing.T, tool, arguments string) []byte {
	t.Helper()
	return writePlanStream(t, structuredToolResponse("responses", tool, arguments))
}

func writePlanStream(t *testing.T, response map[string]any) []byte {
	t.Helper()
	for _, raw := range response["output"].([]any) {
		if item := raw.(map[string]any); item["type"] == "function_call" {
			item["namespace"] = chatGPTToolNamespace
		}
	}
	w := httptest.NewRecorder()
	if err := testutil.WriteInferenceStream(w, "responses", response); err != nil {
		t.Fatal(err)
	}
	return w.Body.Bytes()
}

func planResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(body))}
}

func planTestClient(t *testing.T, cfg ClientConfig, roundTrip planRoundTripper) Client {
	t.Helper()
	cfg.Model, cfg.MaxOutputTokens = "gpt-6.1-sol", 1000
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 1
	}
	c, err := newChatGPTResponsesClient(t.Context(), cfg, func(context.Context) (string, error) { return "oauth-only", nil }, roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatGPTToolLoop(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	t.Setenv("OPENAI_BASE_URL", "https://wrong.invalid/v1")
	t.Setenv("OPENAI_ORG_ID", "ambient-org")
	t.Setenv("OPENAI_PROJECT_ID", "ambient-project")
	t.Setenv("OPENAI_CUSTOM_HEADERS", "Authorization: wrong\nX-Ambient: private")

	// Large tool arguments fit in the next wire request, even though Fantasy
	// also keeps them on the streamed tool call in its internal transcript.
	arguments := `{"key":"first","path":"` + strings.Repeat("x", 8192) + `"}`
	first := structuredToolResponse("responses", "read", arguments)
	first["metadata"] = map[string]string{"path": "a", "Path": "b"}
	second := structuredToolResponse("responses", "read", `{"key":"second","i":1,"I":2,"İ":3}`)["output"].([]any)[0].(map[string]any)
	second["id"], second["call_id"] = "fc-second", "call-second"
	firstOutput := []any{
		map[string]any{"type": "reasoning", "id": "reason-before", "summary": []any{}, "encrypted_content": "opaque-before"},
		second,
		first["output"].([]any)[0],
		map[string]any{"type": "reasoning", "id": "reason-after", "summary": []any{}, "encrypted_content": "opaque-after"},
	}
	first["output"] = firstOutput
	// Deliver the tool calls in reverse order. The completion supplies the
	// order Roast must use for both dispatch and replay.
	const terminalEvent = "event: response.completed\n"
	deltas, _, _ := strings.Cut(string(writePlanStream(t, first)), terminalEvent)
	firstOutput[1], firstOutput[2] = firstOutput[2], firstOutput[1]
	_, completion, _ := strings.Cut(string(writePlanStream(t, first)), terminalEvent)
	firstStream := []byte(deltas + terminalEvent + completion)
	finalStream := planStream(t, "submit", `{"answer":"done"}`)
	streams := [][]byte{firstStream, finalStream}
	wantHistory := append([]any{
		map[string]any{"role": "developer", "content": "system"},
		map[string]any{"role": "user", "content": "user"},
	}, firstOutput...)
	wantHistory = append(wantHistory,
		map[string]any{"type": "function_call_output", "call_id": "call-read", "output": `"file contents"`},
		map[string]any{"type": "function_call_output", "call_id": "call-second", "output": `"file contents"`},
	)

	requests := 0
	var captures []RawResponse
	c := planTestClient(t, ClientConfig{Ledger: NewLedger(5500), CaptureResponse: func(response RawResponse) error {
		captures = append(captures, response)
		return nil
	}}, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != "https://api.openai.com/v1/responses" ||
			req.Header.Get("Authorization") != "Bearer oauth-only" || req.Header.Get("Accept") != "text/event-stream" {
			t.Fatal("wrong request route, authentication, or stream headers")
		}
		for _, header := range []string{"OpenAI-Organization", "OpenAI-Project", "X-Ambient"} {
			if req.Header.Get(header) != "" {
				t.Fatalf("ambient header %s leaked", header)
			}
		}
		var body struct{ Input []any }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if requests == 1 {
			if len(body.Input) < len(wantHistory) || !reflect.DeepEqual(body.Input[:len(wantHistory)], wantHistory) {
				t.Fatal("second request did not replay the finalized output and tool results in order")
			}
			if len(body.Input) != len(wantHistory)+1 {
				t.Error("unexpected input budget warning on the wrap-up request")
			}
		}
		if requests >= len(streams) {
			t.Fatal("unexpected request after final submission")
		}
		resp := planResponse(streams[requests])
		if requests == 0 {
			resp.Header.Del("Content-Type")
		}
		requests++
		return resp, nil
	})
	var out struct{ Answer string }
	var reads []any
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", InputByteCeiling: 10000,
		Tools: basicTools(func(_ context.Context, args map[string]any) (any, error) {
			reads = append(reads, args["key"])
			if len(captures) != 1 {
				t.Error("tool dispatched before response capture")
			}
			return "file contents", nil
		}),
	}, &out)
	if err != nil || out.Answer != "done" || !reflect.DeepEqual(reads, []any{"first", "second"}) || requests != 2 || len(captures) != 2 {
		t.Fatalf("answer=%q reads=%v requests=%d captures=%d error=%v", out.Answer, reads, requests, len(captures), err)
	}
	for i, capture := range captures {
		if capture.Protocol != "chatgpt" || capture.ContentType != "text/event-stream" || !bytes.Equal(capture.Body, streams[i]) {
			t.Fatalf("capture %d did not preserve the event stream", i)
		}
	}
}

func TestChatGPTCredentialFailure(t *testing.T) {
	c, err := newChatGPTResponsesClient(t.Context(), ClientConfig{Protocol: "chatgpt", Model: "test", MaxOutputTokens: 100}, func(context.Context) (string, error) {
		return "", fmt.Errorf("%w: plan permission missing", ErrAuth)
	}, planRoundTripper(func(*http.Request) (*http.Response, error) { t.Fatal("request sent without consent"); return nil, nil }))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Answer string }
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong credential error: %v", err)
	}
}

func TestChatGPTCountsTerminalUsage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		planCode string
		inputKey string
	}{
		{name: "completed", status: "completed"},
		{name: "completed with case variant", status: "completed", inputKey: "Input_Tokens"},
		{name: "incomplete", status: "incomplete"},
		{name: "incomplete with plan error", status: "incomplete", planCode: chatGPTUsageLimit},
		{name: "failed", status: "failed", planCode: chatGPTUsageLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := NewLedger(20_000)
			c := planTestClient(t, ClientConfig{Ledger: ledger}, func(*http.Request) (*http.Response, error) {
				response := structuredToolResponse("responses", "submit", `{"answer":"done"}`)
				inputKey := tc.inputKey
				if inputKey == "" {
					inputKey = "input_tokens"
				}
				response["usage"] = map[string]any{
					inputKey: 100, "input_tokens_details": map[string]any{"cached_tokens": 30}, "output_tokens": 10,
				}
				if tc.status == "completed" {
					return planResponse(writePlanStream(t, response)), nil
				}
				terminal := map[string]any{"status": tc.status, "usage": response["usage"]}
				if tc.planCode != "" {
					terminal["error"] = map[string]string{"code": tc.planCode}
				}
				frame, err := json.Marshal(map[string]any{
					"type": "response." + tc.status, "response": terminal,
				})
				if err != nil {
					t.Fatal(err)
				}
				return planResponse([]byte("data: " + string(frame) + "\n\n")), nil
			})
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			switch {
			case tc.status == "completed":
				if err != nil || out.Answer != "done" {
					t.Fatalf("answer=%q error=%v", out.Answer, err)
				}
			case tc.planCode != "":
				if rejection, ok := errors.AsType[*chatGPTError](err); !ok || rejection.Code != tc.planCode {
					t.Fatalf("lost plan rejection: %v", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "ChatGPT response was incomplete") {
					t.Fatalf("expected an incomplete response, got %v", err)
				}
			}
			u := c.Usage()
			if u.Calls != 1 || u.UncachedInputTokens != 70 || u.CachedInputTokens != 30 || u.OutputTokens != 10 {
				t.Fatalf("usage = %+v", u)
			}
			if spent, _ := ledger.Committed(); spent != 83 {
				t.Fatalf("committed %d tokens, want 83 including discounted cached input", spent)
			}
		})
	}
}

func TestChatGPTCaptureFailurePreventsToolExecution(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wrapUp       bool
		contextError error
		captureError error
	}{
		{name: "caller cancels during exploration", contextError: context.Canceled},
		{name: "caller cancels during wrap-up", contextError: context.Canceled, wrapUp: true},
		{name: "request timeout", contextError: context.DeadlineExceeded},
		{name: "capture rejects authorization", captureError: &fantasy.ProviderError{StatusCode: http.StatusForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				captureErr := tc.captureError
				if captureErr == nil {
					captureErr = errors.New("disk full")
				}
				var requestContext context.Context
				c := planTestClient(t, ClientConfig{RequestTimeout: time.Second, CaptureResponse: func(RawResponse) error {
					switch tc.contextError {
					case context.Canceled:
						cancel()
					case context.DeadlineExceeded:
						<-requestContext.Done()
					}
					return captureErr
				}}, func(req *http.Request) (*http.Response, error) {
					requestContext = req.Context()
					return planResponse(planStream(t, "read", `{}`)), nil
				})
				var out struct{ Answer string }
				opts := ConverseOpts{
					MaxTurns: 2, FinalTool: "submit",
					Tools: basicTools(func(context.Context, map[string]any) (any, error) {
						t.Error("tool dispatched after capture failure")
						return nil, nil
					}),
				}
				if tc.wrapUp {
					opts.WorkDeadline = time.Now().Add(-time.Second)
				}
				err := c.Converse(ctx, "system", "user", opts, &out)
				if !errors.Is(err, captureErr) || errors.Is(err, ErrAuth) || errors.Is(err, ErrWorkDeadline) != tc.wrapUp {
					t.Fatalf("error = %v, want capture failure (at cutoff: %v)", err, tc.wrapUp)
				}
				if tc.contextError != nil && !errors.Is(err, tc.contextError) {
					t.Fatalf("error = %v, want context cause %v", err, tc.contextError)
				}
			})
		})
	}
}

func TestChatGPTRejectedStreamPreservesCaptureAndProviderErrors(t *testing.T) {
	captureErr := errors.New("disk full")
	c := planTestClient(t, ClientConfig{CaptureResponse: func(RawResponse) error { return captureErr }}, func(*http.Request) (*http.Response, error) {
		const event = `{"type":"response.failed","response":{"error":{"code":"generic-provider-code"}},"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`
		return planResponse([]byte("data: " + event + "\n\n")), nil
	})
	var out struct{ Answer string }
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	rejection, ok := errors.AsType[*chatGPTError](err)
	if !errors.Is(err, captureErr) || !ok || rejection.Code != "subscription_sharing_usage_limit_exceeded" {
		t.Fatalf("error = %v, want both the capture and plan errors", err)
	}
}

func TestChatGPTRequestPreviewContract(t *testing.T) {
	const tool = `{"type":"function","name":"submit","parameters":{"type":"object","additionalProperties":false}}`
	const sdkRequest = `{
		"model":"test","tool_choice":"auto","tools":[` + tool + `],
		"input":[{"role":"system","content":"SDK input"}],
		"store":true,"stream":false,"max_output_tokens":100,"previous_response_id":"stored","unknown_sdk_field":true
	}`
	const want = `{
		"model":"test","tool_choice":"auto","store":false,"stream":true,"include":["reasoning.encrypted_content"],
		"input":[{"role":"developer","content":"original prompt"}],
		"tools":[{"type":"namespace","name":"roast","description":"Roast code review and final submission tools.","tools":[` + tool + `]}]
	}`
	prompt := fantasy.Prompt{fantasy.NewSystemMessage("original prompt")}
	wire, err := encodeChatGPTRequest([]byte(sdkRequest), prompt)
	if err != nil {
		t.Fatal(err)
	}
	var got, expected map[string]any
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("request = %s\nwant %s", wire, want)
	}
}

func TestChatGPTRequestRejectsHostedTools(t *testing.T) {
	request := `{"model":"test","tools":[{"type":"tool_search"}]}`
	if _, err := encodeChatGPTRequest([]byte(request), nil); err == nil || !strings.Contains(err.Error(), "local function tools") {
		t.Fatalf("request with a hosted tool returned %v", err)
	}
}

func TestChatGPTStreamBoundsUnterminatedLine(t *testing.T) {
	source := strings.NewReader(strings.Repeat("x", maxCapturedResponseSize+2))
	body := newChatGPTStreamBody(io.NopCloser(source), &chatGPTCall{})
	if _, err := io.Copy(io.Discard, body); err == nil || !strings.Contains(err.Error(), "bounded stream size") {
		t.Fatalf("oversized frame returned %v", err)
	}
	if source.Len() != 1 {
		t.Fatal("oversized frame consumed input beyond the limit sentinel")
	}
}

func TestChatGPTUnsafeStreamsNeverDispatch(t *testing.T) {
	good := string(planStream(t, "read", `{}`))
	terminal := strings.Index(good, "event: response.completed")
	if terminal < 0 {
		t.Fatal("missing fixture completion")
	}
	completion := good[terminal:]
	cases := map[string]string{
		"contradictory-event-name":       good[:terminal] + strings.Replace(completion, "event: response.completed", "event: response.incomplete", 1),
		"truncated":                      good[:terminal],
		"unterminated-completion":        strings.TrimSuffix(good, "\n"),
		"truncated-after-completion":     good + "data: {\"type\":\"response.completed\"}\n",
		"duplicate-completion":           good + completion,
		"duplicate-response":             good[:terminal] + strings.Replace(completion, `"response":{`, `"response":{"error":{"message":"private"}},"response":{`, 1),
		"duplicate-response-alias":       good[:terminal] + strings.Replace(completion, `"response":{`, `"response":{"error":{"message":"private"}},"re\u017fponse":{`, 1),
		"duplicate-response-error":       good[:terminal] + strings.Replace(completion, `"response":{`, `"response":{"error":{"message":"private"},"error":null,`, 1),
		"duplicate-usage-counter":        good[:terminal] + strings.Replace(completion, `"input_tokens":10`, `"input_tokens":100,"input_tokens":10`, 1),
		"contradictory-completion":       good[:terminal] + strings.Replace(completion, `"response":{`, `"response":{"error":{"message":"private marker"},`, 1),
		"incomplete":                     good[:terminal] + "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
		"failed-limit":                   good[:terminal] + "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"limited\"}}}\n\n",
		"explicit-error":                 good[:terminal] + "data: {\"type\":\"error\",\"code\":\"subscription_sharing_usage_unavailable\"}\n\n",
		"wrong-namespace":                strings.ReplaceAll(good, `"namespace":"roast"`, `"namespace":"shell"`),
		"missing-namespace":              strings.ReplaceAll(good, `"namespace":"roast",`, ""),
		"wrong-completed-status":         strings.ReplaceAll(good, `"status":"completed"`, `"status":"failed"`),
		"done-without-complete":          good[:terminal] + "data: [DONE]\n\n",
		"invalid-message-role":           good[:terminal] + "data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"developer"}]}}` + "\n\n",
		"finalized-omitted-tool":         good[:terminal] + "data: " + `{"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n",
		"finalized-changed-tool":         good[:terminal] + strings.Replace(completion, `"arguments":"{}"`, `"arguments":"{\"changed\":true}"`, 1),
		"noncanonical-tool-namespace":    good[:terminal] + strings.Replace(completion, `"namespace":`, `"Namespace":`, 1),
		"duplicate-tool-arguments":       good[:terminal] + strings.Replace(completion, `"arguments":"{}"`, `"arguments":"{}","arguments":"{}"`, 1),
		"duplicate-argument-members":     strings.ReplaceAll(good, `"{}"`, `"{\"path\":\"a\",\"path\":\"b\"}"`),
		"malformed-tool-arguments":       strings.ReplaceAll(good, `"{}"`, `"{"`),
		"non-json-whitespace":            strings.ReplaceAll(good, `"{}"`, `"\u00a0{}"`),
		"duplicate-replay-id-field":      good[:terminal] + strings.Replace(completion, `"type":"function_call"`, `"type":"function_call","id":"duplicate"`, 1),
		"finalized-duplicate-item-id":    good[:terminal] + strings.Replace(completion, `"output":[`, `"output":[{"type":"reasoning","id":"fc-read","summary":[]},`, 1),
		"finalized-unfinished-message":   good[:terminal] + strings.Replace(completion, `"output":[`, `"output":[{"type":"message","role":"assistant","status":"in_progress"},`, 1),
		"finalized-unfinished-reasoning": good[:terminal] + strings.Replace(completion, `"output":[`, `"output":[{"type":"reasoning","status":"incomplete"},`, 1),
		"finalized-null-output":          good[:terminal] + "data: " + `{"type":"response.completed","response":{"status":"completed","output":null}}` + "\n\n",
		"finalized-only-tool":            completion,
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(*http.Request) (*http.Response, error) {
				requests++
				return planResponse([]byte(stream)), nil
			})
			var out struct{ Answer string }
			tools := 0
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
				tools++
				return "wrong", nil
			})}, &out)
			if err == nil || tools != 0 || requests != 1 {
				t.Fatalf("err=%v tools=%d requests=%d", err, tools, requests)
			}
			if name == "failed-limit" && !strings.Contains(err.Error(), "settings/usage") {
				t.Fatalf("no usage recovery: %v", err)
			}
			switch name {
			case "duplicate-argument-members", "malformed-tool-arguments", "non-json-whitespace":
				if !strings.Contains(err.Error(), "contains invalid tool arguments") {
					t.Fatalf("invalid arguments did not reach the guard: %v", err)
				}
			case "duplicate-tool-arguments":
				if !strings.Contains(err.Error(), "invalid ChatGPT stream event") {
					t.Fatalf("duplicate arguments field did not reach the guard: %v", err)
				}
			}
			if strings.HasPrefix(name, "finalized-") && !strings.Contains(err.Error(), "finalized output") {
				t.Fatalf("no finalized output error: %v", err)
			}
			if name == "finalized-omitted-tool" && c.Usage().Calls != 1 {
				t.Fatalf("lost call count after the SDK finished: %+v", c.Usage())
			}
		})
	}
}

func TestChatGPTAdmissionErrors(t *testing.T) {
	const ineligible = `{"error":{"code":"subscription_sharing_user_not_eligible","message":"provider-secret"}}`
	const temporary = `{"error":{"code":"subscription_sharing_usage_unavailable"}}`
	const limit = `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unsupported", 400, `{"error":{"code":"subscription_sharing_unsupported_capability","message":"provider-secret"}}`, "Remove the unsupported capability"},
		{"unauthorized", 401, `{"detail":"provider-secret"}`, "reauthorize it with roast chatgpt signin --account CLIENT_ID"},
		{"unreadable", 401, "", "reauthorize it with roast chatgpt signin --account CLIENT_ID"},
		{"forbidden", 403, `{"detail":"provider-secret"}`, "roast chatgpt signin --account CLIENT_ID --enable-plan"},
		{"ineligible-401", 401, ineligible, "ineligible for plan usage"},
		{"ineligible-403", 403, ineligible, "ineligible for plan usage"},
		{"limited", 429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"provider-secret"}}`, "chatgpt.com/settings/usage"},
		{"redirect", 302, `{"detail":"provider-secret"}`, "HTTP 302"},
		{"unknown code", 503, `{"error":{"code":"provider-secret"}}`, "HTTP 503"},
		{"unknown and temporary", 503, `{"error":{"code":"provider-secret"},"code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"truncated code", 503, `{"error":{"code":`, "HTTP 503"},
		{"invalid code type", 503, `{"error":{"code":123}}`, "HTTP 503"},
		{"null code", 503, `{"error":{"code":null},"code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"temporary and limited", 503, `{"error":{"code":"subscription_sharing_usage_unavailable"},"code":"subscription_sharing_usage_limit_exceeded"}`, "chatgpt.com/settings/usage"},
		{"nested response code", 503, `{"code":"subscription_sharing_usage_unavailable","response":{"code":"subscription_sharing_usage_limit_exceeded"}}`, "chatgpt.com/settings/usage"},
		{"completed response and temporary code", 503, `{"code":"subscription_sharing_usage_unavailable","response":{"status":"completed","usage":{"output_tokens":12}}}`, "HTTP 503"},
		{"usage and temporary code", 503, `{"error":{"code":"subscription_sharing_usage_unavailable"},"usage":{"input_tokens":10}}`, "HTTP 503"},
		{"unknown status and temporary code", 503, `{"status":"unknown","code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"invalid status and temporary code", 503, `{"response":{"status":42,"error":{"code":"subscription_sharing_usage_unavailable"}}}`, "HTTP 503"},
		{"invalid nested response code", 503, `{"code":"subscription_sharing_usage_unavailable","response":{"code":123}}`, "HTTP 503"},
		{"malformed and limited", 503, `{"error":"provider-secret","code":"subscription_sharing_usage_limit_exceeded"}`, "chatgpt.com/settings/usage"},
		{"malformed and temporary", 503, `{"response":"provider-secret","code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"null error and temporary", 503, `{"error":null,"code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"null response and temporary", 503, `{"response":null,"error":{"code":"subscription_sharing_usage_unavailable"}}`, "HTTP 503"},
		{"null response error and temporary", 503, `{"response":{"error":null},"code":"subscription_sharing_usage_unavailable"}`, "HTTP 503"},
		{"message only", 503, `{"error":{"message":"provider-secret"}}`, "HTTP 503"},
		{"invalid UTF-8", 503, `{"error":{"code":"subscription_sharing_usage_unavailable","message":"` + "\xff" + `"}}`, "HTTP 503"},
		{"duplicate codes", 503, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","Code":"subscription_sharing_usage_unavailable"}}`, "HTTP 503"},
		{"duplicate error objects", 503, `{"error":{"code":"subscription_sharing_usage_unavailable"},"Error":{}}`, "HTTP 503"},
		{"duplicate code and permanent code", 503, `{"error":{"code":"a","Code":"b"},"code":"subscription_sharing_usage_limit_exceeded"}`, "HTTP 503"},
		{"unreadable service rejection", 503, "", "HTTP 503"},
		{"oversized rejection", 503, temporary + strings.Repeat(" ", (64<<10)-len(temporary)+1), "HTTP 503"},
		{"oversized plan limit", 503, limit + strings.Repeat(" ", (64<<10)-len(limit)) + "x", "HTTP 503"},
		{"declared oversized plan limit", 429, limit, "HTTP 429"},
		{"short body", 503, temporary, "HTTP 503"},
		{"short plan limit", 429, limit, "chatgpt.com/settings/usage"},
		{"interrupted after error body", 503, `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`, "chatgpt.com/settings/usage"},
		{"interrupted temporary rejection", 503, temporary, "HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(*http.Request) (*http.Response, error) {
				requests++
				var body io.Reader = strings.NewReader(tc.body)
				if strings.HasPrefix(tc.name, "unreadable") || strings.HasPrefix(tc.name, "interrupted") {
					body = io.MultiReader(body, iotest.ErrReader(io.ErrUnexpectedEOF))
				}
				response := &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"X-Request-Id": {"private-header"}, "Location": {"https://wrong.invalid"}},
					Body:       io.NopCloser(body),
				}
				if strings.HasPrefix(tc.name, "short") {
					response.ContentLength = int64(len(tc.body) + 1)
				}
				if tc.name == "declared oversized plan limit" {
					response.ContentLength = (64 << 10) + 1
				}
				return response, nil
			})
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			rejection, ok := errors.AsType[*chatGPTError](err)
			if requests != 1 || !ok || rejection.StatusCode != tc.status || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("requests=%d error=%v, want status %d with %q", requests, err, tc.status, tc.want)
			}
			if rejection.Code == "provider-secret" {
				t.Error("typed error retained the raw provider code")
			}
			if strings.HasPrefix(tc.name, "duplicate") && rejection.Code != chatGPTUnknownCode {
				t.Errorf("used a plan code from a body with duplicate fields: %q", rejection.Code)
			}
			if strings.HasSuffix(tc.name, "oversized plan limit") && rejection.Code != "" {
				t.Errorf("used a plan code from an oversized body: %q", rejection.Code)
			}
			if want := tc.status == 401 || tc.status == 403; errors.Is(err, ErrAuth) != want {
				t.Fatalf("errors.Is(%v, ErrAuth) = %t, want %t", err, !want, want)
			}
			for _, private := range []string{"provider-secret", "private-header", "wrong.invalid", "API key"} {
				if strings.Contains(err.Error(), private) {
					t.Errorf("error exposed %q: %v", private, err)
				}
			}
			if tc.body == ineligible && strings.Contains(err.Error(), "roast chatgpt signin") {
				t.Errorf("eligibility failure suggested repeating OAuth: %v", err)
			}
		})
	}
}

func TestChatGPTConnectionAndReadTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readErr error
		status  int
	}{
		{name: "request deadline", status: http.StatusServiceUnavailable},
		{name: "body read", readErr: context.DeadlineExceeded, status: http.StatusServiceUnavailable},
		{name: "network timeout", readErr: os.ErrDeadlineExceeded, status: http.StatusServiceUnavailable},
		{name: "stream network timeout", readErr: os.ErrDeadlineExceeded, status: http.StatusOK},
		{name: "connection timeout", readErr: os.ErrDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				c := planTestClient(t, ClientConfig{MaxAttempts: 3, RequestTimeout: time.Second}, func(req *http.Request) (*http.Response, error) {
					requests++
					const rejection = `{"error":{"code":"subscription_sharing_usage_unavailable"}}`
					switch tc.status {
					case 0:
						return nil, fmt.Errorf("%w: private body reader details", tc.readErr)
					case http.StatusOK:
						response := planResponse(nil)
						response.Body = io.NopCloser(iotest.ErrReader(tc.readErr))
						return response, nil
					}
					if tc.readErr != nil {
						body := io.MultiReader(strings.NewReader(rejection), iotest.ErrReader(fmt.Errorf("%w: private body reader details", tc.readErr)))
						return &http.Response{StatusCode: tc.status, Body: io.NopCloser(body)}, nil
					}
					body, writer := io.Pipe()
					go func() {
						_, _ = io.WriteString(writer, rejection)
						<-req.Context().Done()
						_ = writer.CloseWithError(req.Context().Err())
					}()
					return &http.Response{StatusCode: tc.status, Body: body}, nil
				})
				var out struct{ Answer string }
				err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
				rejection, ok := errors.AsType[*chatGPTError](err)
				if requests != 1 || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("requests=%d error=%v, want one request and a timeout", requests, err)
				}
				if tc.status == http.StatusServiceUnavailable && (!ok || rejection.Code != chatGPTUsageUnavailable) {
					t.Fatalf("lost the HTTP rejection: %v", err)
				}
				if strings.Contains(err.Error(), "private body reader details") {
					t.Errorf("error exposed body reader details: %v", err)
				}
			})
		})
	}
}

func TestChatGPTTransportErrorsDoNotExposeProviderText(t *testing.T) {
	private := errors.New("private transport diagnostic")
	for _, stage := range []string{"connection", "stream"} {
		t.Run(stage, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(*http.Request) (*http.Response, error) {
				requests++
				if stage == "connection" {
					return nil, private
				}
				response := planResponse(nil)
				response.Body = io.NopCloser(iotest.ErrReader(private))
				return response, nil
			})
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if requests != 1 || !errors.Is(err, ErrEndpoint) || strings.Contains(err.Error(), private.Error()) {
				t.Fatalf("requests=%d error=%v, want one attempt and a sanitized endpoint failure", requests, err)
			}
		})
	}
}

func TestChatGPTStreamingErrorsDoNotExposeProviderText(t *testing.T) {
	const prefix = ": keepalive\n\n"
	for _, event := range []string{
		`{"type":"error","code":"oauth-only","message":"provider-text"}`,
		`{"type":"response.in_progress","error":{"message":"provider-text"}}`,
		`{"type":"response.in_progress","response":{"error":{"message":"provider-text"}}}`,
		`{"type":"response.output_item.added","item":{"type":"oauth-only"}}`,
	} {
		var capture []byte
		c := planTestClient(t, ClientConfig{CaptureResponse: func(response RawResponse) error {
			capture = response.Body
			return nil
		}}, func(*http.Request) (*http.Response, error) {
			return planResponse([]byte(prefix + "data: " + event + "\n\n")), nil
		})
		var out struct{ Answer string }
		err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
		if err == nil || strings.Contains(err.Error(), "oauth-only") || strings.Contains(err.Error(), "provider-text") {
			t.Errorf("error = %v, want a sanitized rejection", err)
		}
		if !bytes.Equal(capture, []byte(prefix)) {
			t.Errorf("capture = %q, want only the accepted prefix %q", capture, prefix)
		}
	}
}

func TestChatGPTRejectsOversizedRestoredHistoryBeforeCredentials(t *testing.T) {
	output := &chatGPTOutput{
		json.RawMessage(`{"type":"reasoning","id":"reason-large","summary":[],"encrypted_content":"` + strings.Repeat("x", defaultInputByteCeiling*4) + `","status":"completed"}`),
	}
	state := &chatGPTCall{prompt: fantasy.Prompt{{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.ReasoningPart{
			ProviderOptions: fantasy.ProviderOptions{chatGPTOutputKey: output},
		}},
	}}}
	ctx := context.WithValue(t.Context(), chatGPTCallKey{}, state)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatGPTBaseURL+"/responses", strings.NewReader(`{"model":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	transport := &chatGPTTransport{
		token: func(context.Context) (string, error) {
			t.Fatal("retrieved credential for oversized restored history")
			return "", nil
		},
		base: planRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("sent oversized restored history")
			return nil, nil
		}),
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil || err == nil || !strings.Contains(err.Error(), "exceeds the bounded input size") || !errors.Is(state.failure, err) {
		t.Fatalf("response=%v error=%v retained failure=%v", resp, err, state.failure)
	}
}

func TestChatGPTStreamAllowsCommentsCRLFAndMultilineData(t *testing.T) {
	stream := ": keepalive\r\n\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"error\":null,\"output\":[]}}\r\n\r\n"
	call := &chatGPTCall{}
	body := newChatGPTStreamBody(io.NopCloser(strings.NewReader(stream)), call)
	got, err := io.ReadAll(body)
	if err != nil || string(got) != stream || call.output == nil {
		t.Fatalf("completed=%v error=%v stream=%q", call.output != nil, err, got)
	}
}

func TestChatGPTRejectsNonStreamingSuccess(t *testing.T) {
	jsonResponse := func(header http.Header) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`))}
	}
	for _, tc := range []struct {
		name     string
		response *http.Response
		want     string
	}{
		{"JSON content type", jsonResponse(http.Header{"Content-Type": {"application/json"}}), "non-streaming"},
		{"empty content type", jsonResponse(http.Header{"Content-Type": {""}}), "non-streaming"},
		{"conflicting content types", jsonResponse(http.Header{"Content-Type": {"text/event-stream", "application/json"}}), "non-streaming"},
		{"no content type", jsonResponse(nil), "without response.completed"},
		{"no content", &http.Response{StatusCode: http.StatusNoContent}, "without response.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := planTestClient(t, ClientConfig{}, func(*http.Request) (*http.Response, error) {
				return tc.response, nil
			})
			var out struct{ Answer string }
			if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestChatGPTInputKeepsLocalNudgeAfterFinalizedOutput(t *testing.T) {
	const reasoning = `{"type":"reasoning","summary":[]}`
	const nudge = `{"arguments":"{}","call_id":"nudge","name":"__roast_continue","namespace":"roast","type":"function_call"}`
	for _, tc := range []struct {
		name  string
		items []json.RawMessage
		want  string
	}{
		{"reasoning", []json.RawMessage{json.RawMessage(reasoning)}, "[" + reasoning + "," + nudge + "]"},
		{"empty", []json.RawMessage{}, "[" + nudge + "]"},
		{"null", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := chatGPTOutput(tc.items)
			prompt := fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "SDK reasoning summary"},
				fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions{chatGPTOutputKey: &output}},
				fantasy.ToolCallPart{ToolCallID: "nudge", ToolName: nudgeToolName, Input: `{}`},
			}}}
			wire, err := encodeChatGPTRequest([]byte(`{"model":"test"}`), prompt)
			if tc.items == nil {
				if err == nil || !strings.Contains(err.Error(), "missing finalized assistant output") {
					t.Fatalf("null assistant output returned %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var request struct{ Input json.RawMessage }
			if err := json.Unmarshal(wire, &request); err != nil {
				t.Fatal(err)
			}
			if string(request.Input) != tc.want {
				t.Fatalf("input = %s, want %s", request.Input, tc.want)
			}
		})
	}
}

func TestChatGPTPlanRejectionAtCutoffOrCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		deadline    time.Duration
		cancel      bool
		finalTurn   bool
		spendBudget bool
	}{
		{name: "temporary rejection during wrap-up", status: 503, deadline: -time.Second},
		{name: "caller cancels during wrap-up", status: 403, deadline: -time.Second, cancel: true},
		{name: "caller cancels during exploration", status: 403, cancel: true},
		{name: "caller cancels after temporary rejection", status: 503, cancel: true},
		{name: "deadline during exploration", status: 403, deadline: time.Second},
		{name: "deadline during final turn", status: 403, deadline: time.Second, finalTurn: true},
		{name: "deadline with exhausted ledger", status: 403, deadline: time.Second, spendBudget: true},
		{name: "deadline and caller cancellation", status: 403, deadline: time.Second, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				requests := 0
				const limit = 5500
				ledger := NewLedger(limit)
				c := planTestClient(t, ClientConfig{MaxAttempts: 3, Ledger: ledger}, func(req *http.Request) (*http.Response, error) {
					requests++
					if requests == 1 && tc.deadline > 0 {
						<-req.Context().Done()
					}
					if tc.cancel {
						cancel()
					}
					if tc.spendBudget {
						release, err := ledger.Reserve(0, 0, true)
						if err != nil {
							t.Fatal(err)
						}
						release(Usage{UncachedInputTokens: limit})
					}
					return &http.Response{StatusCode: tc.status, Body: http.NoBody}, nil
				})
				opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil)}
				if tc.finalTurn {
					opts.MaxTurns = 1
				}
				if tc.deadline != 0 {
					opts.WorkDeadline = time.Now().Add(tc.deadline)
				}
				var out struct{ Answer string }
				err := c.Converse(ctx, "system", "user", opts, &out)
				rejection, ok := errors.AsType[*chatGPTError](err)
				if requests != 1 || ledger.Exhausted() || !ok || rejection.StatusCode != tc.status {
					t.Fatalf("requests=%d reservation refused=%t error=%v, want one rejection with HTTP %d", requests, ledger.Exhausted(), err, tc.status)
				}
				cutoff := tc.deadline != 0
				if errors.Is(err, ErrWorkDeadline) != cutoff || errors.Is(err, ErrNoFinalCall) != cutoff ||
					errors.Is(err, context.Canceled) != tc.cancel || errors.Is(err, ErrAuth) != (tc.status == 403) {
					t.Fatalf("lost error cause: %v", err)
				}
			})
		})
	}
}

func TestChatGPTTemporaryRejectionRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelWait bool
	}{
		{name: "retry with a complete rejection"},
		{name: "caller cancels during the retry wait", cancelWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				requests := 0
				c := planTestClient(t, ClientConfig{MaxAttempts: 2}, func(*http.Request) (*http.Response, error) {
					requests++
					if requests == 1 {
						const body = `{"response":{"error":{"code":"subscription_sharing_usage_unavailable","metadata":{"path":"a","Path":"b"}}}}`
						return &http.Response{
							StatusCode: http.StatusServiceUnavailable, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)),
						}, nil
					}
					return planResponse(planStream(t, "submit", `{"answer":"done"}`)), nil
				})
				if tc.cancelWait {
					go func() {
						synctest.Wait()
						cancel()
					}()
				}
				var out struct {
					Answer string `json:"answer"`
				}
				err := c.CompleteStructured(ctx, "system", "user", "submit", &out)
				if tc.cancelWait {
					if err != context.Canceled || requests != 1 {
						t.Fatalf("requests=%d error=%v, want cancellation during the retry wait", requests, err)
					}
				} else if err != nil || requests != 2 || out.Answer != "done" {
					t.Fatalf("requests=%d answer=%q error=%v", requests, out.Answer, err)
				}
			})
		})
	}
}

func TestChatGPTInvalidSubmissionReportsSchemaError(t *testing.T) {
	c := planTestClient(t, ClientConfig{}, func(*http.Request) (*http.Response, error) {
		return planResponse(planStream(t, "submit", `{"answer":42}`)), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if err == nil || !strings.Contains(err.Error(), "did not satisfy the schema") || strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("invalid ChatGPT submission returned %v", err)
	}
}

func TestChatGPTInterruptionAtWorkDeadline(t *testing.T) {
	for _, tc := range []struct {
		name         string
		transportErr error
		wantWrapUp   bool
	}{
		{name: "request context allows wrap-up", wantWrapUp: true},
		{name: "wrapped request deadline allows wrap-up", transportErr: fmt.Errorf("reading response: %w", context.DeadlineExceeded), wantWrapUp: true},
		{name: "socket timeout does not retry", transportErr: os.ErrDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(req *http.Request) (*http.Response, error) {
					requests++
					if requests == 1 {
						<-req.Context().Done()
						if tc.transportErr != nil {
							return nil, tc.transportErr
						}
						return nil, req.Context().Err()
					}
					return planResponse(planStream(t, "submit", `{"answer":"done"}`)), nil
				})
				var out struct{ Answer string }
				notices := 0
				opts := ConverseOpts{
					MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil), WorkDeadline: time.Now().Add(time.Second),
					OnWorkDeadline: func() { notices++ },
				}
				err := c.Converse(t.Context(), "system", "user", opts, &out)
				if !tc.wantWrapUp {
					if requests != 1 || notices != 1 || !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("requests=%d notices=%d error=%v, want one request and both deadline causes", requests, notices, err)
					}
				} else if err != nil || requests != 2 || notices != 1 || out.Answer != "done" {
					t.Fatalf("requests=%d deadline notices=%d result=%+v error=%v", requests, notices, out, err)
				}
			})
		})
	}
}

func TestChatGPTTransportRejectsUnexpectedRoutes(t *testing.T) {
	transport := &chatGPTTransport{token: func(context.Context) (string, error) { t.Fatal("retrieved token for wrong endpoint"); return "", nil }}
	for _, route := range []struct{ method, url string }{
		{http.MethodPost, "https://wrong.invalid/v1/responses"},
		{http.MethodPost, chatGPTBaseURL + "/models"},
		{http.MethodPost, chatGPTBaseURL + "/responses?query=1"},
		{http.MethodGet, chatGPTBaseURL + "/responses"},
	} {
		req, err := http.NewRequest(route.method, route.url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "permits only POST") {
			t.Fatalf("%s %s: %v, want route rejection", route.method, route.url, err)
		}
	}
}
