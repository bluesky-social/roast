package chatgpt

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
)

// A credential belongs to one registration. Its identity survives sign-out.
type credential struct {
	Account
	Generation        string          `json:"generation,omitempty"`
	IDToken           string          `json:"id_token,omitempty"`
	AccessToken       string          `json:"access_token,omitempty"`
	RefreshToken      string          `json:"refresh_token,omitempty"`
	Scopes            string          `json:"scope,omitempty"`
	EarliestRefreshAt int64           `json:"earliest_refresh_at,omitempty"`
	Pending           *pendingRefresh `json:"pending_refresh,omitempty"`
}

// A refresh can rotate the token before the identity keys are available.
// Keep the response and its receipt time together until validation succeeds.
type pendingRefresh struct {
	Response   oauth.TokenResponse `json:"response"`
	ReceivedAt time.Time           `json:"received_at"`
}

const planScope = "chatgpt.tokens.use.direct"

func (v credential) signedIn() bool {
	return v.AccessToken != "" || v.IDToken != ""
}

func (v credential) hasRevocableGrant() bool {
	token, _ := v.revocationToken()
	return token != ""
}

func (v credential) planEnabled() bool {
	scopes := strings.Fields(v.Scopes)
	return v.AccessToken != "" && slices.Contains(scopes, planScope) && slices.Contains(scopes, "resource.invoke")
}

func (v credential) revocationToken() (token, tokenType string) {
	refresh, access := v.RefreshToken, v.AccessToken
	if v.Pending != nil {
		pending := v.Pending.Response
		if pending.RefreshToken != "" && oauth.ValidRefreshToken(pending.RefreshToken) {
			refresh = pending.RefreshToken
		}
		if pending.AccessToken != "" && oauth.ValidAccessToken(pending.AccessToken) {
			access = pending.AccessToken
		}
	}
	if refresh != "" && oauth.ValidRefreshToken(refresh) {
		return refresh, "refresh_token"
	}
	if access != "" && oauth.ValidAccessToken(access) {
		return access, "access_token"
	}
	return "", ""
}

func (v *credential) clearGrant() {
	v.IDToken = ""
	v.AccessToken = ""
	v.RefreshToken = ""
	v.Scopes = ""
	v.ExpiresAt = time.Time{}
	v.EarliestRefreshAt = 0
	v.Pending = nil
}

func (v *credential) applyRefreshResponse(tokens oauth.TokenResponse, receivedAt time.Time) error {
	if tokens.AccessToken == "" {
		return errors.New("incomplete ChatGPT refresh response: missing access token")
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = v.RefreshToken
	}
	// An omitted scope inherits the grant. An explicit empty scope revokes it.
	if tokens.Scope == nil {
		tokens.Scope = &v.Scopes
	}
	return v.applyTokenResponse(tokens, receivedAt)
}

func (v *credential) applyTokenResponse(tokens oauth.TokenResponse, receivedAt time.Time) error {
	scope := ""
	if tokens.Scope != nil {
		scope = *tokens.Scope
	}
	switch {
	case !oauth.ValidAccessToken(tokens.AccessToken) || !oauth.ValidRefreshToken(tokens.RefreshToken):
		return errors.New("invalid ChatGPT token format")
	case tokens.AccessToken == "" && tokens.IDToken == "":
		return errors.New("incomplete ChatGPT token response")
	case tokens.AccessToken != "" && (!strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 86400):
		return errors.New("incomplete ChatGPT token response")
	case slices.Contains(strings.Fields(scope), planScope) && (tokens.AccessToken == "" || tokens.RefreshToken == ""):
		return errors.New("incomplete ChatGPT plan token response")
	}

	v.AccessToken = tokens.AccessToken
	v.RefreshToken = tokens.RefreshToken
	v.Scopes = scope
	v.ExpiresAt = receivedAt.Add(time.Duration(tokens.ExpiresIn) * time.Second)
	v.EarliestRefreshAt = int64(tokens.EarliestRefreshAt)
	if tokens.IDToken != "" {
		v.IDToken = tokens.IDToken
	}
	return nil
}
