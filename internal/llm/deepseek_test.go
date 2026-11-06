package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/testutil"
)

// deepSeekThinkingServer emulates DeepSeek's thinking mode on its chat and
// Anthropic-compatible endpoints: any forced tool choice is a 400, and every
// earlier assistant turn must replay its reasoning when tools are present.
type deepSeekThinkingServer struct {
	t        *testing.T
	protocol string
	// respond returns the tool to call and its arguments for the nth request.
	// An empty tool answers in prose; a non-empty finish overrides the
	// natural finish reason.
	respond func(request int) (tool, arguments, finish string)

	mu      sync.Mutex
	choices []string
}

const deepSeekToolChoiceError = `{"error":{"message":"Thinking mode does not support this tool_choice (request_id: fixture)","type":"invalid_request_error"}}`

func newDeepSeekThinkingServer(t *testing.T, protocol string, respond func(int) (string, string, string)) (*httptest.Server, *deepSeekThinkingServer) {
	d := &deepSeekThinkingServer{t: t, protocol: protocol, respond: respond}
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	return server, d
}

func (d *deepSeekThinkingServer) reject(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(body))
}

func (d *deepSeekThinkingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Tools      []json.RawMessage `json:"tools"`
		Messages   []map[string]any  `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.choices = append(d.choices, string(request.ToolChoice))
	call := len(d.choices)
	d.mu.Unlock()

	var choice any
	_ = json.Unmarshal(request.ToolChoice, &choice)
	auto := choice == nil || choice == "auto"
	if object, ok := choice.(map[string]any); ok {
		auto = object["type"] == "auto"
	}
	if !auto {
		d.reject(w, deepSeekToolChoiceError)
		return
	}
	for _, message := range request.Messages {
		if message["role"] != "assistant" || len(request.Tools) == 0 {
			continue
		}
		switch d.protocol {
		case "chat":
			if _, ok := message["reasoning_content"]; !ok {
				d.reject(w, `{"error":{"message":"The reasoning_content in the thinking mode must be passed back to the API.","type":"invalid_request_error"}}`)
				return
			}
		case "anthropic":
			content, _ := message["content"].([]any)
			var first map[string]any
			if len(content) > 0 {
				first, _ = content[0].(map[string]any)
			}
			if first["type"] != "thinking" {
				d.reject(w, `{"type":"error","error":{"type":"invalid_request_error","message":"The content[].thinking in the thinking mode must be passed back to the API."}}`)
				return
			}
		}
	}

	tool, arguments, finish := d.respond(call)
	var response map[string]any
	switch d.protocol {
	case "chat":
		message := map[string]any{"role": "assistant", "reasoning_content": "Thinking it through."}
		natural := "stop"
		if tool == "" {
			message["content"] = "Here is my answer in prose."
		} else {
			natural = "tool_calls"
			message["tool_calls"] = []any{map[string]any{
				"id": fmt.Sprintf("call-%d", call), "type": "function",
				"function": map[string]any{"name": tool, "arguments": arguments},
			}}
		}
		if finish == "" {
			finish = natural
		}
		response = map[string]any{
			"id": "chat", "object": "chat.completion", "model": "deepseek-test",
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
	case "anthropic":
		content := []any{map[string]any{"type": "thinking", "thinking": "Thinking it through.", "signature": fmt.Sprintf("ds-%d", call)}}
		natural := "end_turn"
		if tool == "" {
			content = append(content, map[string]any{"type": "text", "text": "Here is my answer in prose."})
		} else {
			natural = "tool_use"
			content = append(content, toolUseBlock(fmt.Sprintf("toolu-%d", call), tool, arguments))
		}
		if finish == "" {
			finish = natural
		}
		response = map[string]any{
			"id": "msg", "type": "message", "role": "assistant", "model": "deepseek-test",
			"content": content, "stop_reason": finish,
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	}
	_ = testutil.WriteInferenceStream(w, d.protocol, response)
}

func deepSeekClient(t *testing.T, server *httptest.Server, protocol string) *client {
	t.Helper()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "deepseek-test",
		Protocol: protocol, MaxOutputTokens: 1024, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*client)
}

// DeepSeek's thinking mode rejects forced tool choice, so every request a
// review makes must leave tool selection automatic: exploration turns,
// natural submissions, the single-call structured path, and each wrap-up.
func TestDeepSeekThinkingModeUsesAutomaticToolChoice(t *testing.T) {
	type answer struct {
		Answer string `json:"answer"`
	}
	exploreThenSubmit := func(request int) (string, string, string) {
		if request == 1 {
			return "read", `{"path":"a.go"}`, ""
		}
		return "submit", `{"answer":"done"}`, ""
	}
	for _, protocol := range []string{"chat", "anthropic"} {
		for _, mode := range []string{"two-turn", "wrap-up", "structured", "structured-retry", "deadline", "budget"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				respond := exploreThenSubmit
				switch mode {
				case "structured", "deadline", "budget":
					respond = func(int) (string, string, string) { return "submit", `{"answer":"done"}`, "" }
				case "structured-retry":
					respond = func(request int) (string, string, string) {
						if request == 1 {
							return "", "", ""
						}
						return "submit", `{"answer":"done"}`, ""
					}
				}
				server, fixture := newDeepSeekThinkingServer(t, protocol, respond)
				c := deepSeekClient(t, server, protocol)
				opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					return "package a", nil
				})}
				switch mode {
				case "wrap-up":
					opts.MaxTurns = 2
				case "deadline":
					opts.WorkDeadline = time.Now().Add(-time.Second)
				case "budget":
					// Refuses exploration but admits the reserved wrap-up call.
					c.cfg.Ledger = NewLedger(4000)
				}
				var out answer
				var err error
				if strings.HasPrefix(mode, "structured") {
					err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
				} else {
					err = c.Converse(t.Context(), "system", "user", opts, &out)
				}
				if err != nil || out.Answer != "done" {
					t.Fatalf("answer=%q error=%v tool choices=%v", out.Answer, err, fixture.choices)
				}
			})
		}
	}
}

func TestDeepSeekFixtureRejectsForcedToolChoice(t *testing.T) {
	for protocol, choices := range map[string][]string{
		"chat":      {`"required"`, `{"type":"function","function":{"name":"submit"}}`},
		"anthropic": {`{"type":"any"}`, `{"type":"tool","name":"submit"}`},
	} {
		server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "submit", `{}`, "" })
		for _, choice := range choices {
			resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{"tool_choice":`+choice+`,"tools":[{}],"messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			var body strings.Builder
			_, _ = io.Copy(&body, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body.String(), "Thinking mode does not support this tool_choice") {
				t.Fatalf("%s %s: status=%d body=%s", protocol, choice, resp.StatusCode, body.String())
			}
		}
	}
}

// When a thinking model's turn ends without a usable submission, the error
// must say why rather than only that the loop ended.
func TestNoSubmissionNamesTheCause(t *testing.T) {
	for _, protocol := range []string{"chat", "anthropic"} {
		t.Run(protocol+"/truncated", func(t *testing.T) {
			finish := "length"
			if protocol == "anthropic" {
				finish = "max_tokens"
			}
			server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "", "", finish })
			var out struct{ Answer string }
			err := deepSeekClient(t, server, protocol).CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), `finish reason "length"`) || !strings.Contains(err.Error(), "max_output_tokens") {
				t.Fatalf("error = %v, want the truncation named", err)
			}
		})
		t.Run(protocol+"/prose", func(t *testing.T) {
			server, _ := newDeepSeekThinkingServer(t, protocol, func(int) (string, string, string) { return "", "", "" })
			var out struct{ Answer string }
			err := deepSeekClient(t, server, protocol).CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if !errors.Is(err, ErrNoFinalCall) || !strings.Contains(err.Error(), "deepseek-test") || !strings.Contains(err.Error(), "2 submission attempts") || !strings.Contains(err.Error(), "without calling `submit`") {
				t.Fatalf("error = %v, want the missing tool call named", err)
			}
		})
	}
	t.Run("provider-rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(deepSeekToolChoiceError))
		}))
		defer server.Close()
		c := deepSeekClient(t, server, "chat")
		var out struct{ Answer string }
		err := c.Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(-time.Second), Tools: basicTools(nil),
		}, &out)
		if !errors.Is(err, ErrWorkDeadline) || !strings.Contains(err.Error(), "Thinking mode does not support this tool_choice") {
			t.Fatalf("error = %v, want the provider's error body", err)
		}
		if strings.HasPrefix(err.Error(), ErrNoFinalCall.Error()) || strings.Count(err.Error(), ErrNoFinalCall.Error()) > 1 {
			t.Fatalf("error = %v, want the provider error ahead of a single generic sentinel", err)
		}
	})
}
