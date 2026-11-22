package llm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/charmbracelet/openai-go/option"

	"github.com/bluesky-social/roast/internal/config"
)

const chatGPTBaseURL = config.OpenAIBaseURL
const chatGPTToolNamespace = "roast"

// newChatGPTResponsesClient connects Fantasy's Responses model to the pinned account.
// The token source marks login and consent failures with ErrAuth.
func newChatGPTResponsesClient(ctx context.Context, cfg ClientConfig, token func(context.Context) (string, error), base http.RoundTripper) (Client, error) {
	if token == nil || base == nil {
		return nil, errors.New("ChatGPT requires a token source and an HTTP transport")
	}
	if cfg.MaxOutputTokens <= 0 {
		return nil, errors.New("max output tokens must be positive")
	}
	cfg.Protocol = "chatgpt"
	capture := &captureTransport{cfg: cfg, base: &chatGPTTransport{base: base, token: token}}
	httpClient := &http.Client{Transport: capture}
	opts := append(deleteHeaders(sdkEnvironmentHeaders("responses")), option.WithMaxRetries(0))
	// The placeholder prevents ambient API credentials from entering the SDK.
	// The transport supplies the selected account's token for each request.
	provider, err := responsesProvider("roast-managed-oauth", chatGPTBaseURL, httpClient, opts...)
	if err != nil {
		return nil, err
	}
	model, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, err
	}
	return &chatGPTClient{client: &client{cfg: cfg, model: &chatGPTModel{LanguageModel: model}, transport: capture}}, nil
}

type chatGPTTransport struct {
	base  http.RoundTripper
	token func(context.Context) (string, error)
}

func (t *chatGPTTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	call, _ := original.Context().Value(chatGPTCallKey{}).(*chatGPTCall)
	req, err := t.prepareRequest(original, call)
	if err != nil {
		return nil, call.recordFailure(err)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if readErr != nil {
			// The HTTP status still determines recovery when the optional error body is incomplete.
			data = nil
		}
		// Handle rejections here so the SDK never sees their raw diagnostics
		// and the HTTP client never follows a redirect.
		return nil, call.recordFailure(newChatGPTError(resp.StatusCode, data))
	}
	if resp.ContentLength > maxCapturedResponseSize {
		_ = resp.Body.Close()
		return nil, call.recordFailure(errors.New("ChatGPT response exceeds the bounded stream size"))
	}
	types := resp.Header.Values("Content-Type")
	if len(types) == 0 {
		// The plan endpoint can omit Content-Type on an SSE response. Let
		// capture and the SDK stream it through the usual completion checks.
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		mediaType, _, err := mime.ParseMediaType(types[0])
		if len(types) != 1 || err != nil || mediaType != "text/event-stream" {
			_ = resp.Body.Close()
			return nil, call.recordFailure(errors.New("ChatGPT returned a non-streaming response"))
		}
	}
	resp.Body = newChatGPTStreamBody(resp.Body, call)
	return resp, nil
}

func (t *chatGPTTransport) prepareRequest(original *http.Request, call *chatGPTCall) (*http.Request, error) {
	if original.Body != nil {
		defer original.Body.Close()
	}
	// Check the destination and the complete replayed input before fetching a token.
	if original.Method != http.MethodPost || original.URL.String() != chatGPTBaseURL+"/responses" {
		return nil, errors.New("chatgpt permits only POST https://api.openai.com/v1/responses")
	}
	if call == nil || original.Body == nil {
		return nil, errors.New("ChatGPT request is missing its conversation history")
	}
	const maxBody = defaultInputByteCeiling * 4
	data, err := io.ReadAll(io.LimitReader(original.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, errors.New("ChatGPT request exceeds the bounded input size")
	}
	body, err := encodeChatGPTRequest(data, call.prompt)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("ChatGPT request exceeds the bounded input size")
	}
	token, err := t.token(original.Context())
	if errors.Is(err, ErrAuth) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("ChatGPT credential: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: no ChatGPT access token; run roast chatgpt signin", ErrAuth)
	}
	req := original.Clone(original.Context())
	req.Host = req.URL.Host
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// chatGPTError keeps the status and code needed for recovery and Fantasy's
// retry policy. Display fixed diagnostics because provider error text and
// headers can contain authentication data.
type chatGPTError struct {
	StatusCode int
	Code       string
}

func newChatGPTError(status int, body []byte) error {
	var wire struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &wire)
	err := &chatGPTError{StatusCode: status, Code: cmp.Or(wire.Error.Code, wire.Code)}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return err
}

func (e *chatGPTError) Error() string {
	detail := fmt.Sprintf("ChatGPT plan usage failed (HTTP %d)", e.StatusCode)
	switch {
	case e.Code == "subscription_sharing_usage_limit_exceeded":
		return detail + ". Pause new plan requests and check https://chatgpt.com/settings/usage"
	case e.Code == "subscription_sharing_user_not_eligible":
		return detail + ". This user, workspace, or policy is ineligible for plan usage."
	case e.Code == "subscription_sharing_unsupported_capability":
		return detail + ". Remove the unsupported capability before retrying."
	case e.StatusCode == http.StatusUnauthorized:
		return detail + ". Check the account with roast chatgpt status and reauthorize it with roast chatgpt signin --account CLIENT_ID"
	case e.StatusCode == http.StatusForbidden:
		return detail + ". If plan permission is missing, request consent with roast chatgpt signin --account CLIENT_ID --enable-plan"
	}
	return detail
}

func (e *chatGPTError) IsRetryable() bool {
	switch e.Code {
	case "", "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		return e.StatusCode == http.StatusServiceUnavailable
	}
	return false
}
