//go:build darwin || linux

package chatgpt

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestSignInCallbackAndPKCE(t *testing.T) {
	c := testClient(t)
	k := testutil.NewRSAKey(t)
	var nonce, redirect, challenge string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case jwksURL:
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(k))
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("client_id") != "oaiapp_test" || r.Form.Get("redirect_uri") != redirect || r.Form.Get("resource") != "https://api.openai.com/v1" {
				t.Error("incorrect exchange")
			}
			verifier := r.Form.Get("code_verifier")
			if len(verifier) < 43 || len(verifier) > 128 {
				t.Error("PKCE verifier length is outside the RFC 7636 range")
			}
			sum := sha256.Sum256([]byte(verifier))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				t.Error("PKCE mismatch")
			}
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: testutil.SignRS256(t, k, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
		default:
			t.Fatal("unexpected destination")
			return nil, nil
		}
	})
	e := c.SignIn(context.Background(), "", func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		if q.Get("client_id") != "dynamic_agent_client" || q.Get("scope") != oauth.Scope || q.Get("agent_name_hint") != "Roast" || q.Get("ext_agent_host_id") == "" {
			t.Error("incorrect authorization")
		}
		redirect = q.Get("redirect_uri")
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		if q.Get("code_challenge_method") != "S256" || nonce == "" || q.Get("state") == "" || nonce == q.Get("state") {
			t.Error("authorization must use S256 and independent state and nonce values")
		}
		if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
			t.Error("not loopback")
		}
		callback, err := url.Parse(redirect)
		if err != nil {
			return err
		}
		bad := url.Values{"state": {"wrong"}, "code": {"bad"}}
		callback.RawQuery = bad.Encode()
		callbackGET(t, callback, http.StatusBadRequest)
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {"oaiapp_test"}}.Encode()
		callbackGET(t, callback, http.StatusOK)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	i, e := c.loadStore()
	if e != nil || i.Active != "oaiapp_test" {
		t.Fatal("registration not activated")
	}
	v, e := c.loadCredential(i.Active)
	if e != nil || v.Subject != "subject" || v.IDToken == "" {
		t.Fatal("identity not stored")
	}
}

func TestIdentityOnlyAndExplicitConsent(t *testing.T) {
	c := testClient(t)
	key := testutil.NewRSAKey(t)
	var nonce string
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		}
		if r.URL.String() != tokenURL {
			t.Fatal("wrong target")
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{IDToken: testutil.SignRS256(t, key, claims(nonce)), Scope: scopeValue("openid profile email")})
	})
	run := func(enable bool) {
		t.Helper()
		show := func(raw string) error {
			u, err := url.Parse(raw)
			if err != nil {
				return err
			}
			q := u.Query()
			nonce = q.Get("nonce")
			if (q.Get("prompt") == "consent") != enable {
				t.Error("forced consent on wrong action")
			}
			if q.Get("scope") != oauth.Scope {
				t.Error("incomplete requested scopes")
			}
			if enable && (q.Get("client_id") != "oaiapp_test" || q.Get("agent_name_hint") != "") {
				t.Error("reauthorization recreated client")
			}
			callback, err := url.Parse(q.Get("redirect_uri"))
			if err != nil {
				return err
			}
			callback.RawQuery = url.Values{"code": {"code"}, "state": {q.Get("state")}, "client_id": {"oaiapp_test"}}.Encode()
			callbackGET(t, callback, http.StatusOK)

			return nil
		}
		var err error
		if enable {
			err = c.EnablePlan(context.Background(), "oaiapp_test", show)
		} else {
			err = c.SignIn(context.Background(), "", show)
		}
		if err == nil || !strings.Contains(err.Error(), "plan usage is disabled") {
			t.Fatalf("missing disabled explanation: %v", err)
		}
	}
	run(false)
	v, err := c.loadCredential("oaiapp_test")
	if err != nil || v.IDToken == "" || v.Subject != "subject" {
		t.Fatal("identity-only sign-in was lost")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 1 || !accounts[0].SignedIn || accounts[0].PlanEnabled {
		t.Fatal("identity-only status incorrect")
	}
	c.selected = "oaiapp_test"
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "--enable-plan") {
		t.Fatalf("identity-only account should require plan consent: %v", err)
	}
	run(true)
}

func TestFailedInitialExchangePreservesPendingRegistration(t *testing.T) {
	c := testClient(t)
	key := testutil.NewRSAKey(t)
	if err := c.withStore(t.Context(), func(accounts *accountStore) error {
		accounts.HostID, accounts.Active = "stable-host", "oaiapp_existing"
		existing := credential{Account: Account{ClientID: "oaiapp_existing", Subject: "existing-subject"}, AccessToken: "existing-access", Scopes: oauth.Scope}
		return accounts.saveCredential(&existing)
	}); err != nil {
		t.Fatal(err)
	}
	var nonce string
	exchanges := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		}
		if r.URL.String() != tokenURL {
			t.Fatal("unexpected OAuth destination")
		}
		exchanges++
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("client_id") != "oaiapp_test" {
			t.Error("exchange did not retain issued client ID")
		}
		if exchanges == 1 {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "access", RefreshToken: "refresh", IDToken: testutil.SignRS256(t, key, claims(nonce)), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
	})
	show := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		nonce = q.Get("nonce")
		expected := "dynamic_agent_client"
		if exchanges > 0 {
			expected = "oaiapp_test"
			if q.Get("agent_name_hint") != "" {
				t.Error("retry used dynamic registration metadata")
			}
		}
		if q.Get("client_id") != expected || q.Get("ext_agent_host_id") != "stable-host" {
			t.Error("retry changed registration or host")
		}
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		callback.RawQuery = url.Values{"code": {"code"}, "state": {q.Get("state")}, "client_id": {"oaiapp_test"}}.Encode()
		callbackGET(t, callback, http.StatusOK)

		return nil
	}
	if err := c.SignIn(context.Background(), "", show); err == nil || !strings.Contains(err.Error(), `account "oaiapp_test"`) || !strings.Contains(err.Error(), "--account CLIENT_ID") {
		t.Fatalf("failed exchange must explain retry: %v", err)
	}
	pending, err := c.loadCredential("oaiapp_test")
	if err != nil || !pending.PendingRegistration || pending.Subject != "" || pending.Email != "" || pending.AccessToken != "" || pending.RefreshToken != "" || pending.IDToken != "" {
		t.Fatal("pending callback was lost or treated as verified identity")
	}
	i, err := c.loadStore()
	if err != nil || i.Active != "oaiapp_existing" || len(i.Accounts) != 2 {
		t.Fatal("failed registration displaced active account")
	}
	if err := c.Select(context.Background(), "oaiapp_test"); err == nil {
		t.Fatal("pending registration became selectable")
	}
	if err := c.SignIn(context.Background(), "oaiapp_test", show); err != nil {
		t.Fatal(err)
	}
	verified, err := c.loadCredential("oaiapp_test")
	if err != nil || verified.PendingRegistration || verified.Subject != "subject" || verified.AccessToken == "" {
		t.Fatal("pending registration did not complete identity validation")
	}
	i, err = c.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if i.Active != "oaiapp_test" || i.HostID != "stable-host" || exchanges != 2 {
		t.Fatal("retry did not retain registration and host")
	}
}

func callbackGET(t *testing.T, callback *url.URL, status int) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil || resp.StatusCode != status {
		t.Fatalf("callback returned %d, want %d: %v", resp.StatusCode, status, err)
	}
}

func TestInterruptedReauthorizationKeepsPreviousAccount(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	err := c.SignIn(ctx, c.selected, func(raw string) error {
		auth, err := url.Parse(raw)
		if err != nil {
			return err
		}
		completeMockCallback(t, auth.Query(), c.selected)
		return nil
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Disconnect Roast in ChatGPT Settings") {
		t.Fatalf("interrupted exchange lost recovery guidance: %v", err)
	}
	previous, err := c.loadCredential(c.selected)
	if err != nil || previous.AccessToken != "access" || previous.RefreshToken != "refresh" {
		t.Fatalf("interrupted exchange changed the previous account: %v", err)
	}
}

// All OAuth traffic uses a synthetic transport; only the callback uses loopback.
func mockSignInTransport(t *testing.T, key *rsa.PrivateKey) transportFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("grant_type") != "authorization_code" {
				t.Error("unexpected grant type")
				return nil, errors.New("unexpected grant type")
			}
			id := r.Form.Get("client_id")
			cl := claims(r.Form.Get("code")) // The fake code carries the test nonce.
			cl["aud"] = id
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "authorized-" + id, RefreshToken: "authorized-refresh-" + id, IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
		case jwksURL:
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		case discoveryURL:
			return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
		case oauth.Issuer + "/revoke":
			return jsonResponse(http.StatusOK, nil)
		default:
			t.Error("unexpected mock OAuth request")
			return nil, errors.New("unexpected request")
		}
	}
}

func beginMockSignIn(t *testing.T, c *Client, id string) (url.Values, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	stopped := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("sign-in goroutine did not stop during cleanup")
		}
	})
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		defer close(stopped)
		done <- c.SignIn(ctx, id, func(raw string) error { ready <- raw; return nil })
	}()
	select {
	case raw := <-ready:
		auth, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return auth.Query(), done
	case err := <-done:
		t.Fatalf("sign-in exited before browser wait: %v", err)
	case <-ctx.Done():
		t.Fatal("sign-in did not reach browser wait")
	}
	return nil, nil
}

func completeMockCallback(t *testing.T, auth url.Values, id string) {
	t.Helper()
	cb, err := url.Parse(auth.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	cb.RawQuery = url.Values{"state": {auth.Get("state")}, "code": {auth.Get("nonce")}, "client_id": {id}}.Encode()
	callbackGET(t, cb, http.StatusOK)
}

func awaitMockSignIn(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("sign-in did not finish")
	}
	return nil
}

func addMockAccount(t *testing.T, c *Client, id string) {
	t.Helper()
	saved := credential{Account: Account{ClientID: id, Subject: "subject", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "access-" + id, RefreshToken: "refresh-" + id, Scopes: oauth.Scope}
	if err := c.saveCredential(&saved); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSignInLeavesAccountOperationsAvailable(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new registration"
		if existing {
			name = "saved registration"
		}
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
			id := ""
			if existing {
				id = "account-b"
				addMockAccount(t, c, id)
			}
			auth, done := beginMockSignIn(t, c, id)
			// A separate client shares only the persisted store, as another CLI does.
			other := *c
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if token, err := other.Token(ctx); err != nil || token != "access" {
				t.Fatalf("pending sign-in blocked token access: %v", err)
			}
			if _, err := other.Status(ctx); err != nil {
				t.Fatalf("pending sign-in blocked status: %v", err)
			}
			if err := other.Select(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked selection: %v", err)
			}
			if err := other.SignOut(ctx, c.selected); err != nil {
				t.Fatalf("pending sign-in blocked sign-out: %v", err)
			}
			completeMockCallback(t, auth, "account-b")
			if err := awaitMockSignIn(t, done); err != nil {
				t.Fatal(err)
			}
			i, err := c.loadStore()
			if err != nil || i.Active != "" || len(i.Accounts) != 2 {
				t.Fatalf("sign-in overwrote sign-out or lost registrations: %v", err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.AccessToken != "" || v.RefreshToken != "" {
				t.Fatal("sign-in resurrected the signed-out account")
			}
			v, err = c.loadCredential("account-b")
			if err != nil || v.AccessToken != "authorized-account-b" {
				t.Fatal("independent sign-in did not persist")
			}
		})
	}
}

func TestConcurrentSignInsMergeRegistrationsAndSelection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	first, firstDone := beginMockSignIn(t, c, "account-b")
	other := *c
	second, secondDone := beginMockSignIn(t, &other, "")
	completeMockCallback(t, second, "account-c")
	if err := awaitMockSignIn(t, secondDone); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, first, "account-b")
	if err := awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	i, err := c.loadStore()
	if err != nil || i.Active != "account-c" || len(i.Accounts) != 3 {
		t.Fatalf("concurrent sign-in lost account updates: %v", err)
	}
}

func TestSignInTokenExpiryAnchoredToReceipt(t *testing.T) {
	c := testClient(t)
	received := time.Now().UTC().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(received.Unix())
	c.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	base := mockSignInTransport(t, testutil.NewRSAKey(t))
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			clock.Store(received.Add(2 * time.Minute).Unix())
		}
		return base(r)
	})
	auth, done := beginMockSignIn(t, c, "")
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	v, err := c.loadCredential("account-b")
	if err != nil || !v.ExpiresAt.Equal(received.Add(time.Hour)) {
		t.Fatalf("JWKS delay extended access-token expiry: %v, %v", v.ExpiresAt, err)
	}
}

// Two processes can complete their browser interactions for the same
// registration, but only the first can consume its code for this generation.
func TestOverlappingSignInsRejectStaleReauthorization(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	base := mockSignInTransport(t, testutil.NewRSAKey(t))
	exchanges := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == tokenURL {
			exchanges++
		}
		return base(r)
	})

	first, firstDone := beginMockSignIn(t, c, c.selected)
	second, secondDone := beginMockSignIn(t, c, c.selected)
	completeMockCallback(t, first, c.selected)
	if err := awaitMockSignIn(t, firstDone); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, second, c.selected)
	if err := awaitMockSignIn(t, secondDone); err == nil || !strings.Contains(err.Error(), "credentials changed") {
		t.Fatalf("stale reauthorization was not rejected: %v", err)
	}
	if exchanges != 1 {
		t.Fatalf("exchanged %d codes, want 1", exchanges)
	}
}

func TestSignInRejectsAccountChangesDuringBrowserWait(t *testing.T) {
	for _, mutation := range []string{"refresh", "sign-out", "host change"} {
		t.Run(mutation, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			base := mockSignInTransport(t, testutil.NewRSAKey(t))
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == tokenURL {
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					if r.Form.Get("grant_type") == "refresh_token" {
						return jsonResponse(http.StatusOK, oauth.TokenResponse{
							AccessToken: "rotated", RefreshToken: "rotated-refresh",
							TokenType: "Bearer", ExpiresIn: 3600,
						})
					}
					t.Error("consumed a code for stale credentials")
				}
				return base(r)
			})

			auth, done := beginMockSignIn(t, c, c.selected)
			wantError := "credentials changed"
			switch mutation {
			case "refresh":
				if token, err := c.Token(t.Context()); token != "rotated" || err != nil {
					t.Fatalf("refresh: %v", err)
				}
			case "sign-out":
				if err := c.SignOut(t.Context(), c.selected); err != nil {
					t.Fatal(err)
				}
			case "host change":
				wantError = "host identity changed"
				if err := c.withStore(t.Context(), func(accounts *accountStore) error {
					accounts.HostID = "different-host"
					return accounts.save(t.Context())
				}); err != nil {
					t.Fatal(err)
				}
			}
			completeMockCallback(t, auth, c.selected)
			if err := awaitMockSignIn(t, done); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("stale authorization was not rejected: %v", err)
			}
		})
	}
}

func TestDiscardedSignInGrantIsRevoked(t *testing.T) {
	for _, failure := range []string{"identity unavailable", "canceled", "canceled during exchange", "canceled after verification", "unwritable store"} {
		t.Run(failure, func(t *testing.T) {
			c := testClient(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			base := mockSignInTransport(t, testutil.NewRSAKey(t))
			revoked := false
			keysReturned := false
			c.now = func() time.Time {
				if failure == "canceled after verification" && keysReturned {
					// ValidateID reads the clock again after verifying the signature.
					cancel()
				}
				return time.Now()
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					if failure == "canceled during exchange" {
						cancel()
						if err := r.Context().Err(); err != nil {
							return nil, err
						}
					}
				case jwksURL:
					keysReturned = true
					saved, err := c.loadCredential("account-b")
					if err != nil || !saved.PendingRegistration || saved.signedIn() || saved.Pending == nil || !saved.Pending.RevokeOnly || saved.Pending.Response.RefreshToken != "authorized-refresh-account-b" {
						t.Fatalf("first grant was not recoverable during identity verification: %v", err)
					}
					switch failure {
					case "identity unavailable":
						return nil, errors.New("identity keys unavailable")
					case "canceled":
						cancel()
						return nil, context.Canceled
					case "unwritable store":
						c.testStoreFaults().write = errors.New("keyring write failed")
					}
				case oauth.Issuer + "/revoke":
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					revoked = r.Form.Get("token") == "authorized-refresh-account-b"
					if failure != "unwritable store" {
						saved, err := c.loadCredential("account-b")
						if err != nil || !saved.PendingRegistration || saved.signedIn() || saved.Pending == nil || !saved.Pending.RevokeOnly || saved.Pending.Response.RefreshToken != r.Form.Get("token") {
							t.Fatalf("rejected first grant could not be recovered for revocation: %v", err)
						}
					}
				}
				return base(r)
			})
			err := c.SignIn(ctx, "", func(raw string) error {
				auth, err := url.Parse(raw)
				if err != nil {
					return err
				}
				completeMockCallback(t, auth.Query(), "account-b")
				return nil
			})
			if err == nil || !revoked {
				t.Fatalf("failed sign-in did not revoke its unpublished grant: %v", err)
			}
			if failure == "canceled after verification" && !errors.Is(err, context.Canceled) {
				t.Fatalf("sign-in returned %v, want cancellation", err)
			}
			accounts, err := c.loadStore()
			if err != nil || accounts.Active != "" || !accounts.Accounts["account-b"].PendingRegistration {
				t.Fatalf("failed sign-in activated an unvalidated grant: %v", err)
			}
		})
	}
}

func TestPendingSignInPreservesExplicitReselection(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = mockSignInTransport(t, testutil.NewRSAKey(t))
	auth, done := beginMockSignIn(t, c, "")
	if err := c.Select(t.Context(), c.selected); err != nil {
		t.Fatal(err)
	}
	completeMockCallback(t, auth, "account-b")
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	accounts, err := c.loadStore()
	if err != nil || accounts.Active != c.selected || !accounts.Accounts["account-b"].signedIn() {
		t.Fatalf("sign-in did not preserve explicit reselection: %v", err)
	}
}

func TestSignInExchangeExcludesRefresh(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	base := mockSignInTransport(t, testutil.NewRSAKey(t))
	exchanging, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == tokenURL {
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				t.Error("refresh reached the provider during code exchange")
				return nil, errors.New("unexpected refresh")
			}
			close(exchanging)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return base(r)
	})

	auth, done := beginMockSignIn(t, c, c.selected)
	completeMockCallback(t, auth, c.selected)
	select {
	case <-exchanging:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not start")
	}
	other := *c
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if token, err := other.Token(ctx); token != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh did not wait for sign-in: %v", err)
	}
	release <- struct{}{}
	if err := awaitMockSignIn(t, done); err != nil {
		t.Fatal(err)
	}
	if token, err := other.Token(t.Context()); token != "authorized-"+c.selected || err != nil {
		t.Fatalf("refresh path did not see the published sign-in: %v", err)
	}
}

func TestCanceledNewCallbackRetainsOnlyRegistrationID(t *testing.T) {
	c := testClient(t)
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("exchanged a code after cancellation")
		return nil, context.Canceled
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- c.SignIn(ctx, "", func(raw string) error { ready <- raw; return nil }) }()
	var raw string
	select {
	case raw = <-ready:
	case err := <-done:
		t.Fatalf("sign-in exited before the callback: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("sign-in never opened the callback")
	}
	auth, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	const id = "issued-on-cancel"
	err = c.withStore(t.Context(), func(*accountStore) error {
		completeMockCallback(t, auth.Query(), id)
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := awaitMockSignIn(t, done); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "--account CLIENT_ID") {
		t.Fatalf("canceled sign-in did not give a way to resume: %v", err)
	}
	accounts, err := c.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	saved := accounts.Accounts[id]
	if accounts.Active != "" || !saved.PendingRegistration || saved.ClientID != id || saved.signedIn() || saved.hasRevocableGrant() {
		t.Fatal("canceled sign-in did not preserve just the pending registration")
	}
	if _, err := c.Token(t.Context()); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "signin --account CLIENT_ID") {
		t.Fatalf("a review did not explain how to resume the saved registration: %v", err)
	}
}

func TestDisplayFailureAfterCallbackSavesOnlyTheIssuedID(t *testing.T) {
	c := testClient(t)
	displayErr := errors.New("could not finish displaying the URL")
	const id = "already-issued"
	err := c.SignIn(t.Context(), "", func(raw string) error {
		auth, err := url.Parse(raw)
		if err != nil {
			return err
		}
		completeMockCallback(t, auth.Query(), id)
		return displayErr
	})
	if !errors.Is(err, displayErr) || !strings.Contains(err.Error(), id) {
		t.Fatalf("missing the issued ID or the display error: %v", err)
	}
	saved, err := c.loadCredential(id)
	if err != nil || !saved.PendingRegistration || saved.signedIn() {
		t.Fatalf("unexpected saved registration after display failed: %v", err)
	}
}

func TestCanceledReauthorizationPreservesExistingRegistration(t *testing.T) {
	c := testClient(t)
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled reauthorization attempted to revoke a potentially shared grant")
		return nil, nil
	})
	for _, token := range []string{"existing", ""} {
		saved := credential{Account: Account{ClientID: "known", Subject: "verified"}, RefreshToken: token}
		err := c.withStore(t.Context(), func(accounts *accountStore) error {
			return c.revokeUnpublishedGrant(t.Context(), accounts, saved, "replacement", "refresh_token", context.Canceled)
		})
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Disconnect Roast in ChatGPT Settings") {
			t.Fatalf("missing cancellation or manual revocation guidance: %v", err)
		}
	}
}
