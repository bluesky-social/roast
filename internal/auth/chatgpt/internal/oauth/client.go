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
	"regexp"
	"strconv"
	"strings"
	"time"

	josejson "github.com/go-jose/go-jose/v4/json"

	"github.com/bluesky-social/roast/internal/config"
)

const (
	Issuer       = "https://auth.openai.com"
	Scope        = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	authorizeURL = Issuer + "/api/accounts/authorize"
	modelsURL    = config.OpenAIBaseURL + "/models"
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

// RefreshTimestamp stores the provider's optional earliest_refresh_at as Unix seconds.
// An invalid hint falls back to token expiry and never discards a rotated grant.
type RefreshTimestamp int64

var refreshEpoch = regexp.MustCompile(`^\+?([0-9]*)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$`)
var refreshDateFraction = regexp.MustCompile(`[.,]0*[1-9]`)

func (t *RefreshTimestamp) UnmarshalJSON(data []byte) error {
	*t = 0
	raw := strings.TrimSpace(string(data))
	if strings.HasPrefix(raw, "\"") {
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		raw = strings.TrimSpace(raw)
	}
	const maxUnix = 253402300799 // Last second of the RFC 3339 year 9999.
	var seconds int64
	if instant, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		seconds = instant.Unix()
		if refreshDateFraction.MatchString(raw) {
			seconds++
		}
	} else if parts := refreshEpoch.FindStringSubmatch(raw); parts != nil {
		digits := strings.TrimLeft(parts[1]+parts[2], "0")
		if digits == "" {
			return nil
		}
		// Only the first twelve integer digits fit a timestamp. Keep the remainder
		// as text so rounding up never loses a fraction or expands a large exponent.
		exponent, _ := strconv.ParseInt(parts[3], 10, 32)
		point := int64(len(digits)-len(parts[2])) + exponent
		switch {
		case point <= 0:
			seconds = 1
		case point <= 12:
			integer := digits[:min(int64(len(digits)), point)]
			integer += strings.Repeat("0", int(point)-len(integer))
			seconds, _ = strconv.ParseInt(integer, 10, 64)
			if point < int64(len(digits)) && strings.ContainsAny(digits[point:], "123456789") {
				seconds++
			}
		}
	}
	if seconds > 0 && seconds <= maxUnix {
		*t = RefreshTimestamp(seconds)
	}
	return nil
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
		if tokens, ok := out.(*TokenResponse); ok && resp.StatusCode/100 == 2 {
			// A nonstandard success is never accepted as a grant. The issuer
			// may still have rotated a token, even if the body is incomplete.
			data, _ := io.ReadAll(io.LimitReader(resp.Body, tokenResponseLimit+1))
			var scanErr error
			_, *tokens, scanErr = scanTokenResponse(data, form.Get("refresh_token"))
			err = fmt.Errorf("%w: unexpected ChatGPT token response (HTTP %d)", ErrNetwork, resp.StatusCode)
			if errors.Is(scanErr, ErrAmbiguousRotation) {
				err = errors.Join(err, scanErr)
			}
		} else {
			err = responseError(resp, target == modelsURL)
		}
	case out != nil:
		if tokens, ok := out.(*TokenResponse); ok {
			previous := form.Get("refresh_token")
			err = readTokenResponse(resp.Body, previous, tokens)
			if err != nil && !errors.Is(err, ErrAmbiguousRotation) && form.Get("grant_type") == "refresh_token" &&
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

// ErrAmbiguousRotation means distinct new refresh tokens were observed but no
// single replacement could be identified for revocation.
var ErrAmbiguousRotation = errors.New("ChatGPT returned conflicting replacement refresh tokens")

// Read one complete document. In particular, a truncated or ambiguous error
// response must never be treated as grounds to discard a refresh token.
func readJSON(body io.Reader, limit int64, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil || int64(len(data)) > limit || json.Unmarshal(data, dst) != nil {
		return errInvalidJSONResponse
	}
	return nil
}

const tokenResponseLimit = 2 << 20

func readTokenResponse(body io.Reader, previousRefresh string, tokens *TokenResponse) error {
	data, readErr := io.ReadAll(io.LimitReader(body, tokenResponseLimit+1))
	grant, issued, scanErr := scanTokenResponse(data, previousRefresh)
	if scanErr != nil {
		*tokens = issued
		return scanErr
	}
	if readErr != nil || len(data) > tokenResponseLimit || !json.Valid(data) ||
		!ValidAccessToken(grant.AccessToken) || !ValidRefreshToken(grant.RefreshToken) {
		*tokens = issued
		return errInvalidJSONResponse
	}
	*tokens = grant
	return nil
}

// Decode each grant field by its exact name. Fully read tokens from a rejected
// response are evidence for revocation only, including when the body is truncated.
func scanTokenResponse(data []byte, previousRefresh string) (grant, issued TokenResponse, err error) {
	var ambiguousAccess, ambiguousRefresh bool
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return grant, issued, errInvalidJSONResponse
	}
	fields := map[string]any{
		"access_token": &grant.AccessToken, "refresh_token": &grant.RefreshToken,
		"id_token": &grant.IDToken, "token_type": &grant.TokenType,
		"expires_in": &grant.ExpiresIn, "scope": &grant.Scope,
	}
	seen := make(map[string]bool)
	valid := true
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			break
		}
		if strings.EqualFold(name, "error") || strings.EqualFold(name, "error_description") || strings.EqualFold(name, "error_uri") {
			// An error envelope can echo request credentials, even on HTTP 200.
			return TokenResponse{}, TokenResponse{}, errInvalidJSONResponse
		}
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			break
		}
		if strings.EqualFold(name, "earliest_refresh_at") {
			const hint = "earliest_refresh_at"
			if !seen[hint] && name == hint {
				_ = grant.EarliestRefreshAt.UnmarshalJSON(raw)
			} else {
				grant.EarliestRefreshAt = 0
			}
			seen[hint] = true
			continue
		}
		wireName := name
		for field := range fields {
			if strings.EqualFold(name, field) {
				name = field
				break
			}
		}
		if seen[name] || name != wireName {
			valid = false
		}
		seen[name] = true
		if dst := fields[name]; dst != nil && josejson.Unmarshal(raw, dst) != nil {
			valid = false
		}
		if name != "access_token" && name != "refresh_token" {
			continue
		}
		var token string
		if json.Unmarshal(raw, &token) != nil || token == "" {
			continue
		}
		switch name {
		case "access_token":
			if ValidAccessToken(token) {
				ambiguousAccess = ambiguousAccess || (issued.AccessToken != "" && issued.AccessToken != token)
				issued.AccessToken = token
			}
		case "refresh_token":
			if token != previousRefresh && ValidRefreshToken(token) {
				ambiguousRefresh = ambiguousRefresh || (issued.RefreshToken != "" && issued.RefreshToken != token)
				issued.RefreshToken = token
			}
		}
	}
	if ambiguousAccess {
		issued.AccessToken = ""
	}
	if ambiguousRefresh {
		// A uniquely identified access token can still be revoked after a new
		// sign-in. The conflicting refresh tokens require manual disconnection.
		issued.RefreshToken = ""
		return grant, issued, ErrAmbiguousRotation
	}

	if !valid {
		return grant, issued, errInvalidJSONResponse
	}
	return grant, issued, nil
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
		"resource":      {config.OpenAIBaseURL},
	})
}

func (c Client) Refresh(ctx context.Context, clientID, refreshToken string) (TokenResponse, error) {
	return c.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {config.OpenAIBaseURL},
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
	if josejson.Unmarshal(data, &envelope) != nil {
		return errInvalidJSONResponse
	}
	for name := range envelope {
		if strings.EqualFold(name, "error") || (name != "models" && strings.EqualFold(name, "models")) {
			return errInvalidJSONResponse
		}
	}
	var entries []*Model
	if json.Unmarshal(envelope["models"], &entries) != nil || entries == nil {
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
