package llm

import (
	"bytes"
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
	headers := sdkEnvironmentHeaders("responses")
	capture := &captureTransport{cfg: cfg, base: &chatGPTTransport{base: base, token: token}}
	httpClient := &http.Client{Transport: capture, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	opts := append(deleteHeaders(headers), option.WithMaxRetries(0))
	// The placeholder prevents ambient API credentials from entering the SDK.
	// The transport supplies the selected account's token for each request.
	provider, err := responsesProvider("roast-managed-oauth", chatGPTBaseURL, httpClient, opts)
	if err != nil {
		return nil, err
	}
	model, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, err
	}
	return &client{cfg: cfg, model: &chatGPTModel{LanguageModel: model}, transport: capture}, nil
}

type chatGPTTransport struct {
	base  http.RoundTripper
	token func(context.Context) (string, error)
}

func (t *chatGPTTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	owner, _ := original.Context().Value(responseBodyKey{}).(*responseBody)
	call, _ := original.Context().Value(chatGPTCallKey{}).(*chatGPTCall)
	req, err := t.prepareRequest(original, call)
	if err != nil {
		return nil, owner.recordFailure(err)
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
		// Redact an echoed bearer before handing the response body to the SDK.
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		data = bytes.ReplaceAll(data, []byte(token), []byte("[redacted]"))
		resp.Body = io.NopCloser(bytes.NewReader(data))
		owner.recordFailure(newChatGPTError(resp.StatusCode, data))
		return resp, nil
	}
	if resp.ContentLength > maxCapturedResponseSize {
		_ = resp.Body.Close()
		return nil, owner.recordFailure(errors.New("ChatGPT response exceeds the bounded stream size"))
	}
	if types := resp.Header.Values("Content-Type"); len(types) == 0 {
		// The plan endpoint can omit Content-Type on an SSE response. Let
		// capture and the SDK stream it through the usual completion checks.
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		resp.Header.Set("Content-Type", "text/event-stream")
	} else if mediaType, _, _ := mime.ParseMediaType(types[0]); mediaType != "text/event-stream" {
		_ = resp.Body.Close()
		return nil, owner.recordFailure(errors.New("ChatGPT returned a non-streaming response; no tools were executed"))
	}
	resp.Body = newChatGPTStreamBody(resp.Body, call, owner)
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
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = nil
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Del("OpenAI-Organization")
	req.Header.Del("OpenAI-Project")
	return req, nil
}

// chatGPTError keeps the status and code needed for recovery and Fantasy's
// retry policy. Display fixed diagnostics because provider error text and
// headers can contain authentication data.
type chatGPTError struct {
	StatusCode int
	Code       string
}

func newChatGPTError(status int, body []byte) *chatGPTError {
	var wire struct {
		Code  string `json:"code"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &wire)
	if wire.Error != nil && wire.Error.Code != "" {
		wire.Code = wire.Error.Code
	}
	return &chatGPTError{StatusCode: status, Code: wire.Code}
}

func (e *chatGPTError) Error() string {
	detail := fmt.Sprintf("ChatGPT plan usage failed (HTTP %d)", e.StatusCode)
	switch e.Code {
	case "subscription_sharing_usage_limit_exceeded":
		detail += "; pause new plan-usage requests and check https://chatgpt.com/settings/usage (the reset time and scope of this limit are not known)"
	case "subscription_sharing_user_not_eligible":
		detail += "; this user, workspace, or policy is not eligible; repeating OAuth will not resolve eligibility"
	case "subscription_sharing_unsupported_capability":
		detail += "; remove the unsupported capability rather than retrying the same request"
	default:
		switch e.StatusCode {
		case http.StatusUnauthorized:
			detail += "; check the selected account with roast chatgpt status and reauthorize it with roast chatgpt signin --account CLIENT_ID"
		case http.StatusForbidden:
			detail += "; check ChatGPT plan permission and account/workspace eligibility; if plan permission is missing, request consent with roast chatgpt signin --account CLIENT_ID --enable-plan"
		}
	}
	return detail
}

func (e *chatGPTError) IsRetryable() bool {
	return e.StatusCode == http.StatusServiceUnavailable && (e.Code == "" || e.Code == "subscription_sharing_usage_unavailable" || e.Code == "subscription_sharing_user_unavailable")
}
