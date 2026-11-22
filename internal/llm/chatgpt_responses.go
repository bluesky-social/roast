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
	"net"
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
	return &client{
		cfg: cfg, model: &chatGPTModel{LanguageModel: model}, transport: capture,
		measurePrompt: chatGPTConversationBytes, fixedOutputLimit: true,
	}, nil
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
		if interruption := chatGPTInterruption(err); interruption != nil {
			return nil, call.recordFailure(interruption)
		}
		return nil, call.recordFailure(errors.New("ChatGPT connection failed before a valid HTTP response"))
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		const maxErrorBody = 64 << 10
		if resp.ContentLength > maxErrorBody {
			_ = resp.Body.Close()
			rejection := permanentModelError{err: newChatGPTError(resp.StatusCode, nil)}
			return nil, call.recordFailure(rejection)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
		_ = resp.Body.Close()
		// Handle rejections here so the SDK never sees their raw diagnostics
		// and the HTTP client never follows a redirect.
		lengthMismatch := resp.ContentLength > 0 && int64(len(data)) != resp.ContentLength
		oversized := len(data) > maxErrorBody
		if oversized || (lengthMismatch && int64(len(data)) > resp.ContentLength) {
			data = nil
		}
		rejection := newChatGPTError(resp.StatusCode, data)
		if readErr != nil || oversized || lengthMismatch {
			rejection = permanentModelError{err: rejection}
		}
		if interruption := chatGPTInterruption(readErr); interruption != nil {
			rejection = providerCallError{fmt.Errorf("%w: %w", rejection, interruption)}
		}
		return nil, call.recordFailure(rejection)
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

func chatGPTInterruption(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if cause == context.DeadlineExceeded {
			return context.DeadlineExceeded
		}
	}
	// A socket timeout can match DeadlineExceeded without coming from the
	// request context. Keep it as a reported failure at the work cutoff.
	networkErr, ok := errors.AsType[net.Error](err)
	if errors.Is(err, context.DeadlineExceeded) || (ok && networkErr.Timeout()) {
		return providerCallError{context.DeadlineExceeded}
	}
	return nil
}

// chatGPTError keeps the status and code needed for recovery and Fantasy's
// retry policy. Display fixed diagnostics because provider error text and
// headers can contain authentication data.
type chatGPTError struct {
	StatusCode int
	Code       string
}

const (
	chatGPTUsageLimit            = "subscription_sharing_usage_limit_exceeded"
	chatGPTUserNotEligible       = "subscription_sharing_user_not_eligible"
	chatGPTUnsupportedCapability = "subscription_sharing_unsupported_capability"
	chatGPTUsageUnavailable      = "subscription_sharing_usage_unavailable"
	chatGPTUserUnavailable       = "subscription_sharing_user_unavailable"
	chatGPTUnknownCode           = "unknown"
)

func newChatGPTError(status int, body []byte) error {
	err := &chatGPTError{StatusCode: status, Code: chatGPTRejectionCode(body)}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return err
}

func chatGPTRejectionCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !uniqueJSONObject(body, nil) {
		return chatGPTUnknownCode
	}
	type envelope struct {
		code, problem, response json.RawMessage
		status, output, usage   json.RawMessage
	}
	malformed, duplicate := false, false
	decode := func(raw json.RawMessage) envelope {
		var result envelope
		if len(raw) == 0 {
			return result
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			malformed = true
			return result
		}
		known := map[string]*json.RawMessage{
			"code": &result.code, "error": &result.problem, "response": &result.response,
			"status": &result.status, "output": &result.output, "usage": &result.usage,
		}
		for name, value := range fields {
			if field := known[foldJSONName(name)]; field != nil {
				if len(*field) != 0 {
					duplicate = true
					return envelope{}
				}
				*field = value
			}
		}
		return result
	}
	// Parse each known location independently so malformed fields cannot
	// authorize retries or hide an actionable code elsewhere in the body.
	root := decode(body)
	directError := decode(root.problem)
	response := decode(root.response)
	responseError := decode(response.problem)
	if duplicate {
		return chatGPTUnknownCode
	}
	possiblyProcessed := false
	for _, field := range []envelope{root, response} {
		var status string
		if len(field.status) != 0 && (json.Unmarshal(field.status, &status) != nil || status != "failed") {
			possiblyProcessed = true
		}
		if jsonValuePresent(field.output) || jsonValuePresent(field.usage) {
			possiblyProcessed = true
		}
	}
	code := knownChatGPTErrorCode(malformed || possiblyProcessed, responseError.code, directError.code, response.code, root.code)
	return cmp.Or(code, chatGPTUnknownCode)
}

func knownChatGPTErrorCode(unknown bool, codes ...json.RawMessage) string {
	var temporary string
	for _, field := range codes {
		if len(field) == 0 {
			continue
		}
		var code string
		if err := json.Unmarshal(field, &code); err != nil {
			unknown = true
			continue
		}
		switch code {
		case chatGPTUsageLimit, chatGPTUserNotEligible, chatGPTUnsupportedCapability:
			return code
		case chatGPTUsageUnavailable, chatGPTUserUnavailable:
			temporary = cmp.Or(temporary, code)
		default:
			unknown = true
		}
	}
	// A 503 with an unknown code stays nonretryable even with another hint.
	if unknown {
		return chatGPTUnknownCode
	}
	return temporary
}

func (e *chatGPTError) Error() string {
	detail := fmt.Sprintf("ChatGPT plan usage failed (HTTP %d)", e.StatusCode)
	switch {
	case e.Code == chatGPTUsageLimit:
		return detail + ". Pause new plan requests and check https://chatgpt.com/settings/usage"
	case e.Code == chatGPTUserNotEligible:
		return detail + ". This user, workspace, or policy is ineligible for plan usage."
	case e.Code == chatGPTUnsupportedCapability:
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
	case "", chatGPTUsageUnavailable, chatGPTUserUnavailable:
		return e.StatusCode == http.StatusServiceUnavailable
	}
	return false
}
