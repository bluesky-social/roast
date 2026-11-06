package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/llm"
	"github.com/bluesky-social/roast/internal/testutil"
)

type scriptedCall struct {
	name string
	args any
}

// scriptedChatClient serves one scripted assistant turn per request over the
// chat protocol. A turn with no calls answers in prose.
func scriptedChatClient(t *testing.T, turns ...[]scriptedCall) (llm.Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(requests.Add(1))
		if n > len(turns) {
			t.Errorf("unexpected request %d", n)
			http.Error(w, "script exhausted", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant"}
		finish := "stop"
		if len(turns[n-1]) == 0 {
			message["content"] = "I have nothing more to add."
		} else {
			finish = "tool_calls"
			var calls []any
			for i, call := range turns[n-1] {
				args, _ := json.Marshal(call.args)
				calls = append(calls, map[string]any{
					"id": fmt.Sprintf("call-%d-%d", n, i), "type": "function",
					"function": map[string]any{"name": call.name, "arguments": string(args)},
				})
			}
			message["tool_calls"] = calls
		}
		_ = testutil.WriteInferenceStream(w, "chat", map[string]any{
			"id": "chat", "object": "chat.completion", "model": "finder-test",
			"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(server.Close)
	client, err := llm.New(t.Context(), llm.ClientConfig{
		BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "finder-test",
		Protocol: "chat", MaxOutputTokens: 1024, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

func wrapUpFinding(file string) WireFinding {
	return WireFinding{
		Reasoning: "reasoning for " + file, Severity: SeverityMedium, Confidence: 0.9,
		Description: "defect in " + file, File: file, Lines: []int{3},
		CodeQuote: "x := 1", FixedCode: "x := 2",
	}
}

// The wrap-up turn keeps every tool on offer so replayed thinking stays
// valid. A finding recorded there must be kept, and recording it must not use
// up the bounded submission attempts; only exploration is refused.
func TestWrapUpTurnKeepsReportedFindings(t *testing.T) {
	a, b := wrapUpFinding("a.go"), wrapUpFinding("b.go")
	submit := func(findings ...WireFinding) scriptedCall {
		return scriptedCall{"submit_findings", FindingsResponse{Assessment: AssessmentFindings, Findings: findings}}
	}
	report := scriptedCall{"report_finding", a}
	explore := scriptedCall{"read_file", map[string]any{"path": "a.go"}}
	cases := []struct {
		name       string
		turns      [][]scriptedCall
		files      []string
		incomplete bool
	}{
		{"report-then-submit", [][]scriptedCall{{report}, {submit(b)}}, []string{"a.go", "b.go"}, false},
		{"report-and-submit-together", [][]scriptedCall{{report, submit(b)}}, []string{"a.go", "b.go"}, false},
		{"report-only", [][]scriptedCall{{report}, {}}, []string{"a.go"}, true},
		{"report-with-exploration", [][]scriptedCall{{explore, report}}, []string{"a.go"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := scriptedChatClient(t, tc.turns...)
			result, err := Find(t.Context(), client, FindInput{
				System: "system", User: "user", Interactive: true, MaxTurns: 1,
				Tools: []llm.Tool{{
					Name: "read_file", Schema: map[string]any{"type": "object"},
					Run: func(context.Context, map[string]any) (any, error) {
						t.Error("exploration ran on the wrap-up turn")
						return nil, nil
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, f := range result.Findings {
				files = append(files, f.File)
			}
			if fmt.Sprint(files) != fmt.Sprint(tc.files) || result.Incomplete != tc.incomplete || int(requests.Load()) != len(tc.turns) {
				t.Fatalf("findings=%v incomplete=%v requests=%d notes=%q", files, result.Incomplete, requests.Load(), result.Notes)
			}
		})
	}
}
