package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

func newChatGPTClient(ctx context.Context, cfg ClientConfig) (Client, error) {
	if err := validateChatGPTConfig(cfg); err != nil {
		return nil, err
	}
	auth, initErr := chatGPTAccount(ctx)
	// Report account failures during inference so a failed verifier leaves the
	// finder results available for reporting and severity gating.
	return newChatGPTResponsesClient(ctx, cfg, func(requestCtx context.Context) (string, error) {
		if initErr != nil {
			return "", fmt.Errorf("%w: %w: initialize Sign in with ChatGPT: %w", ErrAuth, chatgpt.ErrAuth, initErr)
		}
		token, err := auth.Token(requestCtx)
		if errors.Is(err, chatgpt.ErrAuth) {
			return "", fmt.Errorf("%w: %w", ErrAuth, err)
		}
		return token, err
	}, http.DefaultTransport)
}

func validateChatGPTConfig(cfg ClientConfig) error {
	if cfg.APIKey != "" || cfg.SessionHeader != "" || (cfg.BaseURL != "" && cfg.BaseURL != chatGPTBaseURL) {
		return errors.New("chatgpt uses Roast-managed OAuth and the fixed public Responses endpoint; API keys, session headers, and endpoint overrides are not accepted")
	}
	return nil
}

type chatGPTAccountKey struct{}

// The client pins the registration, not its access token. Token refresh and
// sign-out remain effective, but selection changes cannot redirect this work.
// Keep the client private so callers cannot change its authentication state.
type chatGPTAccountSnapshot struct {
	client *chatgpt.Client
	err    error
}

// WithPinnedChatGPTAccount captures the selected account for one operation. Call it
// once after resolving the participating roles and before running any of them,
// including when only a lazy verifier uses ChatGPT. A new call always captures
// the current selection, even if ctx belongs to a previous operation.
//
// This reads local account state without refreshing tokens or making network
// requests. Initialization errors are deferred until a ChatGPT role is
// used, so an unused verifier cannot fail an otherwise successful review.
func WithPinnedChatGPTAccount(ctx context.Context) context.Context {
	client, err := chatgpt.New()
	return context.WithValue(ctx, chatGPTAccountKey{}, chatGPTAccountSnapshot{client, err})
}

func chatGPTAccount(ctx context.Context) (*chatgpt.Client, error) {
	if account, ok := ctx.Value(chatGPTAccountKey{}).(chatGPTAccountSnapshot); ok {
		return account.client, account.err
	}
	return chatgpt.New()
}
