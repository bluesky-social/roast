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

	first := structuredToolResponse("responses", "read", "{}")
	firstOutput := []any{
		map[string]any{"type": "reasoning", "id": "reason-before", "summary": []any{}, "encrypted_content": "opaque-before"},
		first["output"].([]any)[0],
		map[string]any{"type": "reasoning", "id": "reason-after", "summary": []any{}, "encrypted_content": "opaque-after"},
	}
	first["output"] = firstOutput
	firstStream := writePlanStream(t, first)
	finalStream := planStream(t, "submit", `{"answer":"done"}`)
	streams := [][]byte{firstStream, finalStream}
	wantHistory := append([]any{
		map[string]any{"role": "developer", "content": "system"},
		map[string]any{"role": "user", "content": "user"},
	}, firstOutput...)
	wantHistory = append(wantHistory, map[string]any{
		"type": "function_call_output", "call_id": "call-read", "output": `"file contents"`,
	})

	requests := 0
	var captures []RawResponse
	c := planTestClient(t, ClientConfig{CaptureResponse: func(response RawResponse) error {
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
		if requests == 1 && (len(body.Input) < len(wantHistory) || !reflect.DeepEqual(body.Input[:len(wantHistory)], wantHistory)) {
			t.Fatalf("second request did not preserve conversation history\ngot  %v\nwant %v", body.Input, wantHistory)
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
	reads := 0
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			reads++
			if len(captures) != 1 {
				t.Error("tool dispatched before response capture")
			}
			return "file contents", nil
		}),
	}, &out)
	if err != nil || out.Answer != "done" || reads != 1 || requests != 2 || len(captures) != 2 {
		t.Fatalf("answer=%q reads=%d requests=%d captures=%d error=%v", out.Answer, reads, requests, len(captures), err)
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

func TestChatGPTDispatchUsesFinalizedToolOrder(t *testing.T) {
	call := chatGPTCall{completedCalls: []chatGPTOutputItem{
		{CallID: "first", Name: "read", Arguments: "{}"},
		{CallID: "second", Name: "read", Arguments: "{}"},
	}}
	streamed := map[string]fantasy.StreamPart{
		"second": {ID: "second", ToolCallName: "read", ToolCallInput: "{}"},
		"first":  {ID: "first", ToolCallName: "read", ToolCallInput: "{}"},
	}
	ordered, err := call.finalizedToolCalls(streamed)
	if err != nil || len(ordered) != 2 || ordered[0].ID != "first" || ordered[1].ID != "second" {
		t.Fatalf("tool order = %v, error = %v", ordered, err)
	}
}

func TestChatGPTUsageCountsCachedInputOnce(t *testing.T) {
	c := planTestClient(t, ClientConfig{}, func(*http.Request) (*http.Response, error) {
		response := structuredToolResponse("responses", "submit", `{"answer":"done"}`)
		response["usage"] = map[string]any{"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 30}, "output_tokens": 10}
		return planResponse(writePlanStream(t, response)), nil
	})
	var out struct{ Answer string }
	if err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}, &out); err != nil {
		t.Fatal(err)
	}
	usage := c.Usage()
	if usage.UncachedInputTokens != 70 || usage.CachedInputTokens != 30 || usage.OutputTokens != 10 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestChatGPTCountsUnsuccessfulTerminalUsage(t *testing.T) {
	for _, status := range []string{"incomplete", "failed"} {
		t.Run(status, func(t *testing.T) {
			ledger := NewLedger(20_000)
			c := planTestClient(t, ClientConfig{MaxAttempts: 1, Ledger: ledger}, func(*http.Request) (*http.Response, error) {
				frame := fmt.Sprintf("data: "+`{"type":"response.%s","response":{"status":"%s","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":30},"output_tokens":10}}}`+"\n\n", status, status)
				return planResponse([]byte(frame)), nil
			})
			var out struct{ Answer string }
			if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err == nil {
				t.Fatal("accepted rejected response")
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
	c := planTestClient(t, ClientConfig{CaptureResponse: func(RawResponse) error { return errors.New("disk full") }}, func(*http.Request) (*http.Response, error) { return planResponse(planStream(t, "read", `{}`)), nil })
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
		t.Error("tool dispatched after capture failure")
		return nil, nil
	})}, &out)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error=%v", err)
	}
}

func TestChatGPTRejectedStreamPreservesCaptureFailure(t *testing.T) {
	captureErr := errors.New("disk full")
	c := planTestClient(t, ClientConfig{CaptureResponse: func(RawResponse) error { return captureErr }}, func(*http.Request) (*http.Response, error) {
		return planResponse([]byte("data: {\"type\":\"response.incomplete\"}\n\n")), nil
	})
	var out struct{ Answer string }
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, captureErr) {
		t.Fatalf("lost the permanent response capture failure: %v", err)
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
			if strings.HasPrefix(name, "finalized-") && !strings.Contains(err.Error(), "finalized output") {
				t.Fatalf("no finalized output error: %v", err)
			}
		})
	}
}

func TestChatGPTAdmissionErrors(t *testing.T) {
	const ineligible = `{"error":{"code":"subscription_sharing_user_not_eligible","message":"provider-secret"}}`
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(*http.Request) (*http.Response, error) {
				requests++
				var body io.Reader = strings.NewReader(tc.body)
				if tc.name == "unreadable" {
					body = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"X-Request-Id": {"private-header"}, "Location": {"https://wrong.invalid"}},
					Body:       io.NopCloser(body),
				}, nil
			})
			var out struct{ Answer string }
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			rejection, ok := errors.AsType[*chatGPTError](err)
			if requests != 1 || !ok || rejection.StatusCode != tc.status || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("requests=%d error=%v, want status %d with %q", requests, err, tc.status, tc.want)
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

func TestChatGPTStreamingErrorsDoNotExposeProviderText(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","code":"oauth-only","message":"provider-text"}`,
		`{"type":"response.in_progress","error":{"message":"provider-text"}}`,
		`{"type":"response.output_item.added","item":{"type":"oauth-only"}}`,
	} {
		var capture []byte
		c := planTestClient(t, ClientConfig{CaptureResponse: func(response RawResponse) error {
			capture = response.Body
			return nil
		}}, func(*http.Request) (*http.Response, error) {
			return planResponse([]byte("data: " + event + "\n\n")), nil
		})
		var out struct{ Answer string }
		err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
		if err == nil || strings.Contains(err.Error(), "oauth-only") || strings.Contains(err.Error(), "provider-text") ||
			bytes.Contains(capture, []byte("oauth-only")) || bytes.Contains(capture, []byte("provider-text")) {
			t.Error("streamed provider diagnostics escaped into the error or response capture")
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
	stream := ": keepalive\r\n\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output\":[]}}\r\n\r\n"
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := chatGPTOutput(tc.items)
			prompt := fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.ReasoningPart{Text: "SDK reasoning summary"},
				fantasy.ReasoningPart{ProviderOptions: fantasy.ProviderOptions{chatGPTOutputKey: &output}},
				fantasy.ToolCallPart{ToolCallID: "nudge", ToolName: nudgeToolName, Input: `{}`},
			}}}
			wire, err := encodeChatGPTRequest([]byte(`{"model":"test"}`), prompt)
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

func TestChatGPTWrapUpDoesNotRetryPlanRejection(t *testing.T) {
	requests := 0
	c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(*http.Request) (*http.Response, error) {
		requests++
		body := io.NopCloser(strings.NewReader(`{"code":"subscription_sharing_usage_unavailable","error":{}}`))
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: body}, nil
	})
	var out struct{ Answer string }
	opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil), WorkDeadline: time.Now().Add(-time.Second)}
	err := c.Converse(t.Context(), "system", "user", opts, &out)
	if requests != 1 || !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), "ChatGPT plan usage failed (HTTP 503)") {
		t.Fatalf("wrap-up made %d requests, returned %v", requests, err)
	}
}

func TestChatGPTTemporaryRejectionCanRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		c := planTestClient(t, ClientConfig{MaxAttempts: 2}, func(*http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
			}
			return planResponse(planStream(t, "submit", `{"answer":"done"}`)), nil
		})
		var out struct {
			Answer string `json:"answer"`
		}
		if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil || requests != 2 || out.Answer != "done" {
			t.Fatalf("requests=%d answer=%q error=%v", requests, out.Answer, err)
		}
	})
}

func TestChatGPTRejectionAtWorkDeadlineDoesNotStartWrapUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		c := planTestClient(t, ClientConfig{MaxAttempts: 3}, func(req *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				<-req.Context().Done()
			}
			return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: http.NoBody}, nil
		})
		var out struct{ Answer string }
		opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil), WorkDeadline: time.Now().Add(time.Second)}
		err := c.Converse(t.Context(), "system", "user", opts, &out)
		if requests != 1 || !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), "ChatGPT plan usage failed (HTTP 403)") {
			t.Fatalf("made %d requests, returned %v, want one request and a cutoff with the provider rejection", requests, err)
		}
	})
}

func TestChatGPTInterruptedAtWorkDeadlineCanWrapUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		c := planTestClient(t, ClientConfig{}, func(req *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				<-req.Context().Done()
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
		if err := c.Converse(t.Context(), "system", "user", opts, &out); err != nil || requests != 2 || notices != 1 || out.Answer != "done" {
			t.Fatalf("requests=%d deadline notices=%d result=%+v error=%v", requests, notices, out, err)
		}
	})
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
