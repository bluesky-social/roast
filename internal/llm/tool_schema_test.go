package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

// requestToolSchemas returns each tool's JSON Schema as the provider received it.
func requestToolSchemas(t *testing.T, protocol string, request map[string]any) map[string]map[string]any {
	t.Helper()
	tools, _ := request["tools"].([]any)
	schemas := make(map[string]map[string]any, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		var name any
		var schema map[string]any
		switch protocol {
		case "anthropic":
			name, schema = tool["name"], asObject(tool["input_schema"])
		case "responses":
			name, schema = tool["name"], asObject(tool["parameters"])
		default:
			function := asObject(tool["function"])
			name, schema = function["name"], asObject(function["parameters"])
		}
		if name, ok := name.(string); ok {
			schemas[name] = schema
		}
	}
	return schemas
}

func asObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func assertRequired(t *testing.T, what string, schema map[string]any, want ...string) {
	t.Helper()
	raw, _ := schema["required"].([]any)
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		name, _ := item.(string)
		got = append(got, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s required = %v, want %v (schema %v)", what, got, want, schema)
	}
}

// Fantasy's Anthropic adapter reads a tool's required list only as []string
// and sends an empty list for any other shape, so a schema that went through
// JSON must not reach any provider with its required fields dropped.
func TestToolSchemasCarryRequiredFields(t *testing.T) {
	type detail struct {
		Line int    `json:"line"`
		Note string `json:"note,omitempty"`
	}
	type submission struct {
		Answer string `json:"answer"`
		Detail detail `json:"detail"`
		Extra  string `json:"extra,omitempty"`
	}
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			var mu sync.Mutex
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				requests = append(requests, request)
				mu.Unlock()
				_ = testutil.WriteInferenceStream(w, protocol, structuredToolResponse(protocol, "submit", `{"answer":"done","detail":{"line":1}}`))
			}))
			defer server.Close()
			c, err := New(t.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "key", Model: "test-model",
				Protocol: protocol, MaxOutputTokens: 100, MaxAttempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			var out submission
			err = c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
				{Name: "read", Description: "read", Schema: map[string]any{
					"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required": []string{"path"},
				}, Run: func(context.Context, map[string]any) (any, error) { return "package a", nil }},
				{Name: "submit", Description: "submit", Schema: reflectSchema(&out)},
			}}, &out)
			if err != nil || out.Answer != "done" {
				t.Fatalf("answer = %q, error = %v", out.Answer, err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			schemas := requestToolSchemas(t, protocol, requests[0])
			assertRequired(t, "submit", schemas["submit"], "answer", "detail")
			properties := asObject(schemas["submit"]["properties"])
			assertRequired(t, "submit.detail", asObject(properties["detail"]), "line")
			assertRequired(t, "read", schemas["read"], "path")
		})
	}
}
