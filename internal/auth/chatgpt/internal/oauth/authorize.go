package oauth

import (
	"crypto/rand"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/bluesky-social/roast/internal/config"
)

const DynamicClientID = "dynamic_agent_client"

type Authorization struct {
	ClientID string
	HostID   string
	State    string
	Nonce    string
	Verifier string
	Consent  bool
}

// NewHostID identifies this Roast installation to OpenAI across browser sign-ins.
func NewHostID() string {
	return uuid.New().URN()
}

// NewAuthorization binds a fresh state, OIDC nonce, and PKCE verifier to one sign-in.
func NewAuthorization(clientID, hostID string, consent bool) Authorization {
	return Authorization{
		ClientID: clientID,
		HostID:   hostID,
		Consent:  consent,
		State:    rand.Text(),
		Nonce:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
	}
}

// URL omits any retained ID token because the URL may appear in terminal scrollback.
func (a Authorization) URL(redirect string) string {
	client := oauth2.Config{
		ClientID:    a.ClientID,
		RedirectURL: redirect,
		Endpoint:    oauth2.Endpoint{AuthURL: authorizeURL},
		Scopes:      strings.Fields(Scope),
	}
	options := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(a.Verifier),
		oauth2.SetAuthURLParam("nonce", a.Nonce),
		oauth2.SetAuthURLParam("resource", config.OpenAIBaseURL),
		oauth2.SetAuthURLParam("ext_agent_host_id", a.HostID),
	}
	if client.ClientID == "" {
		client.ClientID = DynamicClientID
		options = append(options, oauth2.SetAuthURLParam("agent_name_hint", "Roast"))
	}
	if a.Consent {
		options = append(options, oauth2.SetAuthURLParam("prompt", "consent"))
	}
	return client.AuthCodeURL(a.State, options...)
}
