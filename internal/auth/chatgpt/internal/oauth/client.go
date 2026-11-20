// Package oauth implements OpenAI's token, identity, and model catalog endpoints.
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	josejson "github.com/go-jose/go-jose/v4/json"
)

const (
	Issuer       = "https://auth.openai.com"
	Scope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	authorizeURL = Issuer + "/api/accounts/authorize"
	modelsURL    = APIBaseURL + "/models"
	tokenURL     = Issuer + "/api/accounts/oauth/token"
	jwksURL      = Issuer + "/.well-known/jwks.json"
	discoveryURL = Issuer + "/.well-known/openid-configuration"
)

type Client struct {
	HTTP *http.Client
}

func NewClient() Client {
	return Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type TokenResponse struct {
	AccessToken       string           `json:"access_token"`
	RefreshToken      string           `json:"refresh_token"`
	IDToken           string           `json:"id_token"`
	TokenType         string           `json:"token_type"`
	ExpiresIn         int64            `json:"expires_in"`
	Scope             *string          `json:"scope,omitempty"`
	EarliestRefreshAt RefreshTimestamp `json:"earliest_refresh_at"`
}

func (c Client) request(ctx context.Context, method, target string, form url.Values, bearer string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("cannot prepare ChatGPT request")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// Keep the issuer's redirect policy even when a caller supplies its own HTTP client.
	var client http.Client
	if c.HTTP != nil {
		client = *c.HTTP
	}
	if client.Timeout <= 0 || client.Timeout > 30*time.Second {
		client.Timeout = 30 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: check connectivity and retry", ErrNetwork)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode != http.StatusOK:
		err = responseError(resp, target == modelsURL)
	case out != nil:
		if tokens, ok := out.(*TokenResponse); ok {
			previous := form.Get("refresh_token")
			err = readTokenResponse(resp.Body, previous, tokens)
			if err != nil && form.Get("grant_type") == "refresh_token" &&
				(tokens.RefreshToken == "" || tokens.RefreshToken == previous) {
				err = fmt.Errorf("%w: malformed ChatGPT refresh response. Existing grant retained: %w", ErrNetwork, err)
			}
		} else {
			err = readJSON(resp.Body, 2<<20, out)
		}
	}
	// Preserve successfully read tokens even if the context was just canceled.
	// A refresh may already have consumed the original token.
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}

var errInvalidJSONResponse = errors.New("invalid ChatGPT response")

// Read one complete document. In particular, a truncated or ambiguous error
// response must never be treated as grounds to discard a refresh token.
func readJSON(body io.Reader, limit int64, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || int64(len(data)) > limit || json.Unmarshal(data, dst) != nil {
		return errInvalidJSONResponse
	}
	return nil
}

func readTokenResponse(body io.Reader, previousRefresh string, tokens *TokenResponse) error {
	const limit = 2 << 20
	data, readErr := io.ReadAll(io.LimitReader(body, limit+1))
	issued, rejected := extractCleanupTokens(data, previousRefresh)
	if readErr != nil || len(data) > limit || rejected || !json.Valid(data) || josejson.Unmarshal(data, tokens) != nil ||
		!ValidAccessToken(tokens.AccessToken) || !ValidRefreshToken(tokens.RefreshToken) {
		*tokens = issued
		return errors.New("invalid ChatGPT response")
	}
	return nil
}

// Fully read token fields in a rejected response are evidence for cleanup
// only. Retain a distinct replacement even when a duplicate obscures it.
func extractCleanupTokens(data []byte, previousRefresh string) (TokenResponse, bool) {
	var issued, aliases TokenResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return issued, false
	}
	seen := make(map[string]bool, 2)
	rejected := false
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			break
		}
		if strings.EqualFold(name, "error") || strings.EqualFold(name, "error_description") || strings.EqualFold(name, "error_uri") {
			// An error envelope can echo request credentials, even on HTTP 200.
			return TokenResponse{}, true
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			break
		}
		wireName := name
		name = tokenResponseField(name)
		if name == "" {
			continue
		}
		rejected = rejected || seen[name] || name != wireName
		seen[name] = true
		if name != "access_token" && name != "refresh_token" {
			continue
		}
		var token string
		if json.Unmarshal(raw, &token) != nil || token == "" {
			continue
		}
		candidate := &issued
		if wireName != name {
			candidate = &aliases
		}
		switch name {
		case "access_token":
			if candidate.AccessToken == "" && ValidAccessToken(token) {
				candidate.AccessToken = token
			}
		case "refresh_token":
			if (candidate.RefreshToken == "" || candidate.RefreshToken == previousRefresh) && ValidRefreshToken(token) {
				candidate.RefreshToken = token
			}
		}
	}
	// Prefer an exact-name replacement in either field order. An alias is
	// only a fallback for cleanup, never evidence for publishing a grant.
	if issued.AccessToken == "" {
		issued.AccessToken = aliases.AccessToken
	}
	if issued.RefreshToken == previousRefresh {
		issued.RefreshToken = ""
	}
	if issued.RefreshToken == "" && aliases.RefreshToken != previousRefresh {
		issued.RefreshToken = aliases.RefreshToken
	}

	return issued, rejected
}

func tokenResponseField(name string) string {
	for _, field := range []string{"access_token", "refresh_token", "id_token", "token_type", "expires_in", "scope", "earliest_refresh_at"} {
		if strings.EqualFold(name, field) {
			return field
		}
	}
	return ""
}

// ValidAccessToken accepts an omitted value or the Bearer token syntax.
func ValidAccessToken(token string) bool {
	if token == "" {
		return true
	}
	base := strings.TrimRight(token, "=")
	return base != "" && strings.IndexFunc(base, func(r rune) bool {
		return !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~+/", r)
	}) < 0
}

// ValidRefreshToken accepts an omitted value or a nonblank opaque OAuth token.
func ValidRefreshToken(token string) bool {
	return (token == "" || strings.TrimSpace(token) != "") &&
		strings.IndexFunc(token, func(r rune) bool { return r < 0x20 || r > 0x7e }) < 0
}

func (c Client) Exchange(ctx context.Context, clientID, code, verifier, redirect string) (TokenResponse, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirect},
		"resource":      {APIBaseURL},
	})
}

func (c Client) Refresh(ctx context.Context, clientID, refreshToken string) (TokenResponse, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {APIBaseURL},
	})
}

func (c Client) token(ctx context.Context, form url.Values) (TokenResponse, error) {
	var token TokenResponse
	err := c.request(ctx, http.MethodPost, tokenURL, form, "", &token)
	if err == nil && token.AccessToken == "" && token.IDToken == "" && token.RefreshToken == "" {
		return TokenResponse{}, fmt.Errorf("%w: ChatGPT token endpoint returned no grant", ErrNetwork)
	}
	return token, err
}

func (c Client) Revoke(ctx context.Context, clientID, token, tokenType string) error {
	var discovery struct {
		Issuer     string `json:"issuer"`
		Revocation string `json:"revocation_endpoint"`
	}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, discoveryURL, nil, "", &raw); err != nil {
		return err
	}
	if josejson.Unmarshal(raw, &discovery) != nil {
		return errors.New("invalid OpenAI revocation discovery")
	}
	endpoint, err := url.Parse(discovery.Revocation)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "auth.openai.com" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || discovery.Issuer != Issuer {
		return errors.New("invalid OpenAI revocation discovery")
	}
	form := url.Values{
		"token":           {token},
		"token_type_hint": {tokenType},
		"client_id":       {clientID},
	}
	for attempt := 1; ; attempt++ {
		err := c.request(ctx, http.MethodPost, discovery.Revocation, form, "", nil)
		if !errors.Is(err, ErrNetwork) || attempt == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

type Model struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
}

type modelCatalog []Model

func (catalog *modelCatalog) UnmarshalJSON(data []byte) error {
	var envelope map[string]json.RawMessage
	if josejson.Unmarshal(data, &envelope) != nil || envelope["error"] != nil {
		return errInvalidJSONResponse
	}
	field, value := uniqueJSONField(data, "models")
	var entries []*Model
	if field != "models" || json.Unmarshal(value, &entries) != nil || entries == nil {
		return errInvalidJSONResponse
	}
	decoded := make(modelCatalog, len(entries))
	for index, entry := range entries {
		if entry == nil || (entry.Visibility == "list" && strings.TrimSpace(entry.Slug) == "") {
			return errInvalidJSONResponse
		}
		decoded[index] = *entry
	}
	*catalog = decoded
	return nil
}

func (c Client) Models(ctx context.Context, token string) ([]Model, error) {
	var models modelCatalog
	err := c.request(ctx, http.MethodGet, modelsURL, nil, token, &models)
	if errors.Is(err, errInvalidJSONResponse) {
		return nil, fmt.Errorf("%w: model catalog: %w", ErrNetwork, err)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return models, nil
}
