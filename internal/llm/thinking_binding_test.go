package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

// bindingServer is a native Anthropic Messages fixture that enforces signed
// thinking-block binding the way Claude 5.1+ models do: a block stays valid
// only while the system prompt, the tool set, and every content block before
// it are unchanged. Content is compared after a JSON round trip, so key order
// and whitespace are ignored but any added, removed, edited, or reordered
// block is a mismatch.
type bindingServer struct {
	t       *testing.T
	respond func(request int) []any

	mu       sync.Mutex
	requests []bindingRequest
	issued   map[string]string
}

type bindingRequest struct {
	System     json.RawMessage `json:"system"`
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Messages   []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	} `json:"messages"`
}

func newBindingServer(t *testing.T, respond func(request int) []any) *httptest.Server {
	b := &bindingServer{t: t, respond: respond, issued: map[string]string{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	return server
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Errorf("decode request JSON: %v", err)
		return string(raw)
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func (b *bindingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request bindingRequest
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)

	var prefix bytes.Buffer
	prefix.WriteString(canonicalJSON(b.t, request.System) + "\n" + canonicalJSON(b.t, request.Tools) + "\n")
	for i, message := range request.Messages {
		prefix.WriteString(message.Role + ":")
		for j, block := range message.Content {
			var head struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			_ = json.Unmarshal(block, &head)
			if head.Type == "thinking" {
				if want, ok := b.issued[head.Signature]; !ok || want != prefix.String() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					message := fmt.Sprintf("messages.%d.content.%d: Invalid `signature` in `thinking` block. The block is bound to a different conversation. Remove the block, or set `thinking.block_binding.prefix_mismatch_behavior` to \"drop_block\". That setting requires the `thinking-binding-controls-2026-08-01` value in the `anthropic-beta` header.", i, j)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": message}})
					return
				}
			}
			prefix.WriteString(canonicalJSON(b.t, block) + "\n")
		}
	}

	content := b.respond(len(b.requests))
	prefix.WriteString("assistant:")
	stop := "end_turn"
	for _, raw := range content {
		block := raw.(map[string]any)
		switch block["type"] {
		case "thinking":
			signature := fmt.Sprintf("sig-%d", len(b.issued)+1)
			block["signature"] = signature
			b.issued[signature] = prefix.String()
		case "tool_use":
			stop = "tool_use"
		}
		data, _ := json.Marshal(block)
		prefix.WriteString(canonicalJSON(b.t, data) + "\n")
	}
	_ = testutil.WriteInferenceStream(w, "anthropic", map[string]any{
		"id": fmt.Sprintf("msg-%d", len(b.requests)), "type": "message", "role": "assistant", "model": "claude-test",
		"content": content, "stop_reason": stop,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func thinkingBlock(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text}
}

func toolUseBlock(id, name, arguments string) map[string]any {
	var input map[string]any
	_ = json.Unmarshal([]byte(arguments), &input)
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func bindingClient(t *testing.T, server *httptest.Server) Client {
	t.Helper()
	c, err := New(t.Context(), ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test",
		Protocol: "anthropic", MaxOutputTokens: 1024, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Every follow-up request must replay what each signed thinking block was
// produced under: the same system prompt, the same tools, and the same
// earlier content, including any reminder Roast added to the conversation.
func TestAnthropicReplayKeepsSignedThinkingPrefix(t *testing.T) {
	readTool := func(reads *int) Tool {
		return Tool{Name: "read", Description: "read a file", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
		}, Run: func(_ context.Context, args map[string]any) (any, error) {
			*reads++
			return map[string]any{"content": "package " + fmt.Sprint(args["path"])}, nil
		}}
	}
	submitTool := Tool{Name: "submit", Description: "return the result", Schema: map[string]any{
		"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
		"required": []string{"answer"},
	}}
	type answer struct {
		Answer string `json:"answer"`
	}

	t.Run("structured-retry", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock(""), toolUseBlock("toolu-1", "submit", `{"answer":42}`)}
			}
			return []any{thinkingBlock(""), toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var out answer
		if err := bindingClient(t, server).CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil || out.Answer != "done" {
			t.Fatalf("answer=%q error=%v", out.Answer, err)
		}
	})

	t.Run("wrap-up-turn", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock("look first"), toolUseBlock("toolu-1", "read", `{"path":"a"}`)}
			}
			return []any{thinkingBlock(""), toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 2, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 1 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("interleaved-thinking", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{
					thinkingBlock("first"), toolUseBlock("toolu-a", "read", `{"path":"a"}`),
					thinkingBlock("between tools"), toolUseBlock("toolu-b", "read", `{"path":"b"}`),
				}
			}
			return []any{toolUseBlock("toolu-c", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 4, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 2 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("input-budget-warning", func(t *testing.T) {
		server := newBindingServer(t, func(request int) []any {
			if request < 3 {
				return []any{thinkingBlock(""), toolUseBlock(fmt.Sprintf("toolu-%d", request), "read", `{"path":"a"}`)}
			}
			return []any{toolUseBlock("toolu-3", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 6, FinalTool: "submit", InputByteCeiling: 1, Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})

	t.Run("final-turn-exploration", func(t *testing.T) {
		// The model ignores the wrap-up notice once. Its exploration call is
		// answered, not executed, and the replay keeps the call it made.
		server := newBindingServer(t, func(request int) []any {
			if request == 1 {
				return []any{thinkingBlock(""), toolUseBlock("toolu-1", "read", `{"path":"a"}`), thinkingBlock("after the call")}
			}
			return []any{toolUseBlock("toolu-2", "submit", `{"answer":"done"}`)}
		})
		var reads int
		var out answer
		err := bindingClient(t, server).Converse(t.Context(), "system", "user", ConverseOpts{
			MaxTurns: 1, FinalTool: "submit", Tools: []Tool{readTool(&reads), submitTool},
		}, &out)
		if err != nil || out.Answer != "done" || reads != 0 {
			t.Fatalf("answer=%q reads=%d error=%v", out.Answer, reads, err)
		}
	})
}

func TestBindingServerRejectsChangedPrefix(t *testing.T) {
	server := newBindingServer(t, func(int) []any {
		return []any{thinkingBlock(""), toolUseBlock("toolu-1", "read", `{}`)}
	})
	post := func(body string) (int, string) {
		resp, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	user := `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
	if code, _ := post(`{"system":[],"tools":[],"messages":[` + user + `]}`); code != http.StatusOK {
		t.Fatalf("first request status = %d", code)
	}
	assistant := `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-1"},{"type":"tool_use","id":"toolu-1","name":"read","input":{}}]}`
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu-1","content":"x"}]}`
	if code, body := post(`{"system":[],"tools":[],"messages":[` + user + `,` + assistant + `,` + result + `]}`); code != http.StatusOK {
		t.Fatalf("unchanged replay status = %d: %s", code, body)
	}
	dropped := `{"role":"user","content":[{"type":"text","text":"a"}]}`
	code, body := post(`{"system":[],"tools":[],"messages":[` + dropped + `,` + assistant + `,` + result + `]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "messages.1.content.0: Invalid `signature` in `thinking` block") {
		t.Fatalf("changed prefix status = %d: %s", code, body)
	}
}
