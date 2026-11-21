//go:build darwin || linux

package chatgpt

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
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

func TestIssuedClientIDOpaqueCallback(t *testing.T) {
	for _, tt := range []struct {
		name  string
		id    string
		valid bool
	}{
		{"opaque ID", "issued-client-v2", true},
		{"example prefix", "oaiapp_example", true},
		{"registration placeholder", "dynamic_agent_client", false},
		{"empty", "", false},
		{"whitespace", "has space", false},
		{"control character", "line\nbreak", false},
		{"too long", strings.Repeat("a", 257), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			exchanged := false
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected destination")
				}
				exchanged = true
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				if r.Form.Get("client_id") != tt.id {
					t.Error("issued ID was modified")
				}
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			})
			err := c.SignIn(context.Background(), "", func(raw string) error {
				u, err := url.Parse(raw)
				if err != nil {
					return err
				}
				q := u.Query()
				callback, err := url.Parse(q.Get("redirect_uri"))
				if err != nil {
					return err
				}
				callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"code"}, "client_id": {tt.id}}.Encode()
				callbackGET(t, callback, http.StatusOK)

				return nil
			})
			if err == nil {
				t.Fatal("expected rejected callback or mocked exchange failure")
			}
			if exchanged != tt.valid {
				t.Fatalf("exchange=%t, valid=%t", exchanged, tt.valid)
			}
			if tt.valid {
				pending, e := c.loadCredential(tt.id)
				if e != nil || !pending.PendingRegistration || pending.ClientID != tt.id {
					t.Fatal("opaque issued ID not retained")
				}
			}
		})
	}
}

func TestSignInContextInteractionWindow(t *testing.T) {
	before := time.Now()
	ctx, cancel := signInContext(context.Background())
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(30*time.Minute)) || deadline.After(after.Add(30*time.Minute)) {
		t.Fatal("sign-in interaction deadline is not 30 minutes")
	}
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in context ignored cancellation")
	}
}

func TestSignInContextPreservesCallerDeadlineAndCancellation(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()
	ctx, cancel := signInContext(parent)
	defer cancel()
	actual, ok := ctx.Deadline()
	if !ok || !actual.Equal(deadline) {
		t.Fatal("sign-in extended an earlier caller deadline")
	}
	cancelParent()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in ignored parent cancellation")
	}
}

func startAsyncSignIn(t *testing.T, c *Client, ctx context.Context) (*url.URL, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
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
		done <- c.SignIn(ctx, "oaiapp_test", func(raw string) error { ready <- raw; return nil })
	}()
	select {
	case raw := <-ready:
		auth, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		cb, err := url.Parse(auth.Query().Get("redirect_uri"))
		if err != nil {
			t.Fatal(err)
		}
		cb.RawQuery = url.Values{"state": {auth.Query().Get("state")}, "code": {"private-code-marker"}}.Encode()
		return cb, done
	case err := <-done:
		t.Fatalf("sign-in exited early: %v", err)
	case <-ctx.Done():
		t.Fatal("sign-in did not start")
	}
	return nil, nil
}

func callbackGET(t *testing.T, cb *url.URL, status int) string {
	t.Helper()
	_, body := callbackGETResponse(t, cb, status)
	return body
}

func callbackGETResponse(t *testing.T, cb *url.URL, status int) (http.Header, string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(cb.String())
	if err != nil {
		t.Fatalf("callback HTTP response failed: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("incomplete callback response: %v", err)
	}
	if res.StatusCode != status {
		t.Fatalf("callback status %d, want %d", res.StatusCode, status)
	}
	if status == http.StatusOK {
		assertCallbackCompletePage(t, res.Header, string(body))
	}
	return res.Header, string(body)
}

func TestAsyncCallbackCompleteResponseOnFailure(t *testing.T) {
	for _, kind := range []string{"callback", "exchange"} {
		t.Run(kind, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("private-network-marker") })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cb, done := startAsyncSignIn(t, c, ctx)
			q := cb.Query()
			q.Set("error_description", `<script>alert("private-description-marker")</script>`)
			q.Set("error_uri", "https://private-error-uri.invalid/")
			q.Set("user_content", "private-user-content-marker")
			if kind == "callback" {
				q.Del("code")
				q.Set("error", "private-error-marker")
			}
			cb.RawQuery = q.Encode()
			header, body := callbackGETResponse(t, cb, 200)
			var response bytes.Buffer
			response.WriteString(body)
			if err := header.Write(&response); err != nil {
				t.Fatal(err)
			}
			for _, values := range q {
				for _, value := range values {
					if strings.Contains(response.String(), value) {
						t.Fatal("callback response exposed request data")
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected failure")
				}
			case <-ctx.Done():
				t.Fatal("sign-in stuck")
			}
			cancel()
		})
	}
}

func TestAsyncCallbackOAuthErrorCategories(t *testing.T) {
	for _, tc := range []struct{ name, code, category string }{
		{"denied", "access_denied", "access_denied"},
		{"server", "server_error", "server_error"},
		{"unknown", "private-error-marker", "oauth_error"},
		{"injected", "access_denied\nprivate-error-marker", "oauth_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("OAuth error callback started a token exchange")
				return nil, errors.New("unexpected exchange")
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cb, done := startAsyncSignIn(t, c, ctx)
			q := cb.Query()
			q.Set("error", tc.code)
			q.Set("error_description", "private-description-marker")
			q.Set("error_uri", "https://private-error-uri.invalid/")
			q.Set("request_id", "req_private_callback_marker")
			cb.RawQuery = q.Encode()
			header, body := callbackGETResponse(t, cb, http.StatusOK)
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal("sign-in stuck after OAuth error callback")
			}
			if err == nil || err.Error() != "ChatGPT authorization failed ("+tc.category+")" {
				t.Fatal("callback did not return the fixed OAuth error category")
			}
			var output bytes.Buffer
			output.WriteString(err.Error())
			output.WriteString(body)
			if err := header.Write(&output); err != nil {
				t.Fatal(err)
			}
			for key, values := range q {
				if key == "error" && tc.code == tc.category {
					continue // Only a recognized fixed error category may be shown.
				}
				for _, value := range values {
					if strings.Contains(output.String(), value) {
						t.Fatal("OAuth error callback exposed request data")
					}
				}
			}
		})
	}
}

func TestAsyncCallbackRejectionsAndDuplicate(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	exchange := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		close(exchange)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil, errors.New("mock failure")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cb, done := startAsyncSignIn(t, c, ctx)
	for _, rejection := range []struct{ method, host, event string }{
		{http.MethodPost, cb.Host, "rejected_method"},
		{http.MethodGet, "wrong-host.invalid", "rejected_host"},
	} {
		req, err := http.NewRequest(rejection.method, cb.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = rejection.host
		req.Header.Set("Authorization", "Bearer private-auth-marker")
		req.Header.Set("Cookie", "session=private-cookie-marker")
		res, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("accepted %s", rejection.event)
		}
	}
	invalid := *cb
	invalid.Path = "/private-path-marker"
	callbackGET(t, &invalid, 404)
	invalid = *cb
	q := invalid.Query()
	q.Set("state", "private-state-marker")
	invalid.RawQuery = q.Encode()
	callbackGET(t, &invalid, 400)
	callbackGET(t, cb, 200)
	select {
	case <-exchange:
	case <-ctx.Done():
		t.Fatal("exchange not started")
	}
	callbackGET(t, cb, 400)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Disconnect Roast in ChatGPT Settings") {
			t.Fatalf("interrupted exchange lost recovery guidance: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation stuck")
	}
}

func assertCallbackCompletePage(t *testing.T, header http.Header, body string) {
	t.Helper()
	const wantBody = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Authorization received</title>
<script>history.replaceState(null,'','/auth/complete');</script>
</head>
<body><p>Authorization received. Return to Roast to check the result.</p></body>
</html>
`
	if body != wantBody {
		t.Fatal("callback completion page changed or included request data")
	}
	_, scriptAndRest, ok := strings.Cut(body, "<script>")
	if !ok {
		t.Fatal("callback URL cleanup script missing")
	}
	script, _, ok := strings.Cut(scriptAndRest, "</script>")
	if !ok || script != "history.replaceState(null,'','/auth/complete');" {
		t.Fatal("callback script does not replace the URL with the fixed completion path")
	}
	hash := sha256.Sum256([]byte(script))
	policy := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	for name, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": policy,
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := header.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("unexpected callback %s header", name)
		}
	}
	if header.Get("Location") != "" || header.Get("Set-Cookie") != "" {
		t.Fatal("callback completion page redirects or sets cookies")
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
			if failure == "unwritable store" && os.Geteuid() == 0 {
				t.Skip("requires Unix permission enforcement for the current user")
			}
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
						if err := os.Chmod(c.dir, 0500); err != nil {
							return nil, err
						}
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
			t.Cleanup(func() { _ = os.Chmod(c.dir, 0700) })

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
