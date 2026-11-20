package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestChatGPTRejectsOverridesAndUnexpectedRoutes(t *testing.T) {
	for _, cfg := range []ClientConfig{{APIKey: "secret"}, {BaseURL: "https://chatgpt.com/backend-api/codex"}, {SessionHeader: "X-Session"}} {
		cfg.Protocol, cfg.Model, cfg.MaxOutputTokens = "chatgpt", "test", 100
		if _, err := New(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "not accepted") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid config error=%v", err)
		}
	}
	transport := &chatGPTTransport{token: func(context.Context) (string, error) { t.Fatal("retrieved token for wrong endpoint"); return "", nil }}
	for _, endpoint := range []string{"https://wrong.invalid/v1/responses", chatGPTBaseURL + "/models", chatGPTBaseURL + "/responses?query=1"} {
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(req); err == nil {
			t.Fatal("accepted wrong endpoint")
		}
	}
}
