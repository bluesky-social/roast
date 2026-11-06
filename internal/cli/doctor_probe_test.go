package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/testutil"
)

var doctorTokenPattern = regexp.MustCompile(`roast-doctor-[A-Z2-7]+`)

// doctorProbeServer answers the roundtrip probe the way a thinking model
// would: the first turn calls the lookup tool (with a signed thinking block on
// the Anthropic protocol), the second submits the token it got back. It
// rejects forced tool choice, as DeepSeek's thinking mode does, and rejects a
// follow-up whose tools or opening message differ from the first request, as
// Claude 5 does for signed thinking.
type doctorProbeServer struct {
	t        *testing.T
	protocol string

	mu         sync.Mutex
	requests   int
	firstTools string
	firstInput string
	token      string
}

func newDoctorProbeServer(t *testing.T, protocol string) (*httptest.Server, *doctorProbeServer) {
	d := &doctorProbeServer{t: t, protocol: protocol}
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	return server, d
}

func (d *doctorProbeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(`{"data":[]}`))
		return
	}
	body, _ := io.ReadAll(r.Body)
	var request struct {
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Tools      json.RawMessage   `json:"tools"`
		Messages   []json.RawMessage `json:"messages"`
		Input      []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reject := func(message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
	}
	choice := strings.TrimSpace(string(request.ToolChoice))
	if choice != `"auto"` && !strings.Contains(choice, `"type":"auto"`) {
		reject("Thinking mode does not support this tool_choice")
		return
	}
	opening := append(request.Messages, request.Input...)
	if len(opening) == 0 {
		reject("no messages")
		return
	}

	d.mu.Lock()
	d.requests++
	call := d.requests
	if call == 1 {
		d.firstTools, d.firstInput = string(request.Tools), string(opening[0])
	} else if string(request.Tools) != d.firstTools || string(opening[0]) != d.firstInput {
		d.mu.Unlock()
		reject("messages.1.content.0: Invalid `signature` in `thinking` block. The block is bound to a different conversation.")
		return
	}
	token := doctorTokenPattern.FindString(string(body))
	d.token = token
	d.mu.Unlock()

	tool, arguments := "doctor_lookup", `{}`
	if token != "" {
		tool, arguments = "doctor_probe", fmt.Sprintf(`{"ok":true,"token":%q}`, token)
	}
	w.Header().Set("Content-Type", "application/json")
	var response map[string]any
	switch d.protocol {
	case "anthropic":
		var input map[string]any
		_ = json.Unmarshal([]byte(arguments), &input)
		stop := "tool_use"
		response = map[string]any{
			"id": "msg", "type": "message", "role": "assistant", "model": "probe-model",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": fmt.Sprintf("sig-%d", call)},
				map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu-%d", call), "name": tool, "input": input},
			},
			"stop_reason": stop, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	case "chat":
		response = map[string]any{
			"id": "chat", "object": "chat.completion", "model": "probe-model",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "reasoning_content": "Following the instructions.",
				"tool_calls": []any{map[string]any{
					"id": fmt.Sprintf("call-%d", call), "type": "function",
					"function": map[string]any{"name": tool, "arguments": arguments},
				}},
			}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
	case "responses":
		response = map[string]any{
			"id": fmt.Sprintf("resp-%d", call), "object": "response", "created_at": 1,
			"status": "completed", "model": "probe-model",
			"output": []any{map[string]any{
				"id": fmt.Sprintf("fc-%d", call), "type": "function_call", "call_id": fmt.Sprintf("call-%d", call),
				"name": tool, "arguments": arguments, "status": "completed",
			}},
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		}
	}
	_ = testutil.WriteInferenceStream(w, d.protocol, response)
}

// The probe must exercise what a review relies on: a tool call, its result
// replayed on the next request, and a final submission, all under automatic
// tool choice and an unchanged conversation prefix.
func TestDoctorRoundtripRunsTwoTurnToolLoop(t *testing.T) {
	for _, protocol := range []string{"anthropic", "chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			server, fixture := newDoctorProbeServer(t, protocol)
			result := doctorRoundtripCheck(t.Context(), "finder", config.ResolvedRole{
				BaseURL: server.URL + "/v1", APIKey: "secret", Model: "probe-model",
				Protocol: protocol, MaxOutputTokens: 32768,
			})
			if result.Status != doctorPass || fixture.requests != 2 || fixture.token == "" {
				t.Fatalf("result=%#v requests=%d token=%q", result, fixture.requests, fixture.token)
			}
		})
	}
}

func TestDoctorRoundtripRequiresTheToolResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "chat", `{"id":"chat","object":"chat.completion","model":"probe-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"doctor_probe","arguments":"{\"ok\":true,\"token\":\"guessed\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	result := doctorRoundtripCheck(t.Context(), "verifier", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Model: "probe-model", Protocol: "chat", MaxOutputTokens: 1024,
	})
	if result.Status != doctorFail || !strings.Contains(result.Detail, "doctor_lookup") {
		t.Fatalf("result = %#v, want a failure naming the skipped tool call", result)
	}
}
