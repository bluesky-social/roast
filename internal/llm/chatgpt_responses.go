package llm

import (
	"context"
	"errors"
	"net/http"

	fantasyopenai "charm.land/fantasy/providers/openai"
	"github.com/charmbracelet/openai-go/option"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

const chatGPTBaseURL = chatgpt.APIBaseURL
const chatGPTToolNamespace = "roast"

// newChatGPTResponsesClient connects Fantasy's Responses model to the pinned account.
// The outer transport records the same stream that the model consumes.
func newChatGPTResponsesClient(ctx context.Context, cfg ClientConfig, token func(context.Context) (string, error), base http.RoundTripper) (Client, error) {
	if token == nil || base == nil {
		return nil, errors.New("ChatGPT requires a token source and an HTTP transport")
	}
	if cfg.MaxOutputTokens <= 0 {
		return nil, errors.New("max output tokens must be positive")
	}
	cfg.Protocol = "chatgpt"
	headers := sdkEnvironmentHeaders("responses")
	capture := &captureTransport{cfg: cfg, base: &chatGPTTransport{base: base, token: token}}
	httpClient := &http.Client{Transport: capture, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	opts := append(deleteHeaders(headers), option.WithMaxRetries(0))
	provider, err := fantasyopenai.New(
		// A nonempty placeholder prevents the SDK from selecting ambient API
		// credentials. The transport replaces it on every request, after refresh.
		fantasyopenai.WithAPIKey("roast-managed-oauth"),
		fantasyopenai.WithBaseURL(chatGPTBaseURL),
		fantasyopenai.WithHTTPClient(httpClient),
		fantasyopenai.WithUserAgent(userAgent()),
		fantasyopenai.WithSDKOptions(opts...),
		fantasyopenai.WithUseResponsesAPI(),
		fantasyopenai.WithResponsesAPIFunc(func(string) bool { return true }),
	)
	if err != nil {
		return nil, err
	}
	model, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, err
	}
	return &client{cfg: cfg, model: &chatGPTModel{LanguageModel: model}, transport: capture}, nil
}
