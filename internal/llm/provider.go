package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	fantasyanthropic "charm.land/fantasy/providers/anthropic"
	fantasyopenai "charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

const (
	defaultMaxAttempts      = 4
	defaultInputByteCeiling = 3_000_000
	maxCapturedResponseSize = 16 << 20
)

// ClientConfig configures one finder or verifier model.
type ClientConfig struct {
	Role            string
	BaseURL         string
	APIKey          string
	Model           string
	MaxOutputTokens int
	MaxAttempts     int
	RequestTimeout  time.Duration
	Protocol        string
	Ledger          *Ledger
	CaptureResponse func(RawResponse) error
}

// RawResponse is a successful provider body without requests or headers.
type RawResponse struct {
	Role     string
	Protocol string
	Model    string
	Body     json.RawMessage
}

type client struct {
	cfg   ClientConfig
	model fantasy.LanguageModel

	mu    sync.Mutex
	usage Usage
}

// New constructs the Fantasy provider model selected by cfg.Protocol.
func New(ctx context.Context, cfg ClientConfig) (Client, error) {
	if cfg.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("%s: max output tokens must be positive", cfg.Model)
	}
	httpClient := providerHTTPClient(cfg)
	var (
		provider fantasy.Provider
		err      error
	)
	switch cfg.Protocol {
	case "", "chat":
		provider, err = openaicompat.New(
			openaicompat.WithAPIKey(cfg.APIKey),
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithHTTPClient(httpClient),
			openaicompat.WithUserAgent("roast"),
		)
	case "responses":
		provider, err = fantasyopenai.New(
			fantasyopenai.WithAPIKey(cfg.APIKey),
			fantasyopenai.WithBaseURL(cfg.BaseURL),
			fantasyopenai.WithHTTPClient(httpClient),
			fantasyopenai.WithUserAgent("roast"),
			fantasyopenai.WithUseResponsesAPI(),
			fantasyopenai.WithResponsesAPIFunc(func(string) bool { return true }),
		)
	case "anthropic":
		// The Anthropic SDK appends /v1/messages itself; Roast profiles use the
		// same base-URL convention as the OpenAI protocols and include /v1.
		baseURL := strings.TrimSuffix(strings.TrimRight(cfg.BaseURL, "/"), "/v1")
		provider, err = fantasyanthropic.New(
			fantasyanthropic.WithAPIKey(cfg.APIKey),
			fantasyanthropic.WithBaseURL(baseURL),
			fantasyanthropic.WithHTTPClient(httpClient),
			fantasyanthropic.WithUserAgent("roast"),
		)
	default:
		return nil, fmt.Errorf("unsupported inference protocol %q", cfg.Protocol)
	}
	if err != nil {
		return nil, fmt.Errorf("initialize %s provider for model %q: %w", protocolName(cfg), cfg.Model, err)
	}
	mdl, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("initialize %s model %q: %w", protocolName(cfg), cfg.Model, err)
	}
	return &client{cfg: cfg, model: mdl}, nil
}

func (c *client) Name() string { return c.cfg.Model }

func (c *client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

func (c *client) addUsage(providerUsage fantasy.Usage) Usage {
	input := max(int(providerUsage.InputTokens), 0)
	cacheRead := max(int(providerUsage.CacheReadTokens), 0)
	cacheWrite := max(int(providerUsage.CacheCreationTokens), 0)
	uncached := input
	// OpenAI-shaped protocols report cached input as a subset of input_tokens;
	// Anthropic reports input, cache-read, and cache-creation as disjoint counts.
	if c.cfg.Protocol == "" || c.cfg.Protocol == "chat" || c.cfg.Protocol == "responses" {
		cacheRead = min(cacheRead, input)
		cacheWrite = min(cacheWrite, input-cacheRead)
		uncached = input - cacheRead - cacheWrite
	}
	u := Usage{
		UncachedInputTokens: uncached,
		CachedInputTokens:   cacheRead,
		CacheWriteTokens:    cacheWrite,
		OutputTokens:        max(int(providerUsage.OutputTokens), 0),
		Calls:               1,
	}
	c.mu.Lock()
	c.usage.UncachedInputTokens += u.UncachedInputTokens
	c.usage.CachedInputTokens += u.CachedInputTokens
	c.usage.CacheWriteTokens += u.CacheWriteTokens
	c.usage.OutputTokens += u.OutputTokens
	c.usage.Calls += u.Calls
	c.mu.Unlock()
	return u
}

func maxAttempts(cfg ClientConfig) int {
	if cfg.MaxAttempts > 0 {
		return cfg.MaxAttempts
	}
	return defaultMaxAttempts
}

func protocolName(cfg ClientConfig) string {
	if cfg.Protocol == "" {
		return "chat"
	}
	return cfg.Protocol
}

type captureError struct{ err error }

func (e *captureError) Error() string { return e.err.Error() }
func (e *captureError) Unwrap() error { return e.err }

type captureTransport struct {
	base   http.RoundTripper
	cfg    ClientConfig
	mu     sync.Mutex
	failed error
}

func (t *captureTransport) fail(err error) error {
	wrapped := &captureError{err: err}
	t.mu.Lock()
	t.failed = wrapped
	t.mu.Unlock()
	return wrapped
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	failed := t.failed
	t.mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || t.cfg.CaptureResponse == nil {
		return resp, err
	}
	if resp.ContentLength > maxCapturedResponseSize {
		_ = resp.Body.Close()
		return nil, t.fail(fmt.Errorf("capture %s provider response: body exceeds %d-byte limit", t.cfg.Role, maxCapturedResponseSize))
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCapturedResponseSize+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(body) > maxCapturedResponseSize {
		return nil, t.fail(fmt.Errorf("capture %s provider response: body exceeds %d-byte limit", t.cfg.Role, maxCapturedResponseSize))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if !json.Valid(body) {
		return nil, t.fail(fmt.Errorf("capture %s provider response: %s returned invalid raw JSON", t.cfg.Role, t.cfg.Model))
	}
	captureErr := t.cfg.CaptureResponse(RawResponse{
		Role: t.cfg.Role, Protocol: protocolName(t.cfg), Model: t.cfg.Model,
		Body: append(json.RawMessage(nil), body...),
	})
	if captureErr == nil {
		return resp, nil
	}
	return nil, t.fail(fmt.Errorf("capture %s provider response: %w", t.cfg.Role, captureErr))
}

func providerHTTPClient(cfg ClientConfig) *http.Client {
	client := &http.Client{Transport: &captureTransport{base: http.DefaultTransport, cfg: cfg}}
	if cfg.Protocol == "anthropic" {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func normalizeProviderError(ctx context.Context, modelName string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s: %w", ErrEndpoint, modelName, err)
	}
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrEndpoint) || errors.Is(err, ErrBudgetExhausted) {
		return err
	}
	if status, ok := errorStatus(err); ok && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		return fmt.Errorf("%w: %v; check the configured API key", ErrAuth, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrEndpoint, modelName, err)
}

// errorStatus extracts public status fields from provider errors without
// coupling the rest of Roast to a provider SDK's error type.
func errorStatus(err error) (int, bool) {
	for err != nil {
		v := reflect.ValueOf(err)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		if v.IsValid() && v.Kind() == reflect.Struct {
			for _, name := range []string{"StatusCode", "HTTPStatusCode"} {
				field := v.FieldByName(name)
				if field.IsValid() && field.Kind() == reflect.Int && field.CanInt() && field.Int() > 0 {
					return int(field.Int()), true
				}
			}
		}
		err = errors.Unwrap(err)
	}
	return 0, false
}
