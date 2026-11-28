//go:build darwin || linux

package chatgpt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestRefreshSerializedAndRotated(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	var calls atomic.Int32
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != tokenURL {
			t.Error("wrong token target")
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != c.selected || r.Form.Get("scope") != "" {
			t.Error("bad refresh form")
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "next-access", RefreshToken: "next-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			token, e := c.Token(context.Background())
			if e != nil || token != "next-access" {
				t.Errorf("token refresh failed: %v", e)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}

	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatalf("load rotated credential: %v", err)
	}
	if saved.AccessToken != "next-access" {
		t.Errorf("persisted access token = %q, want next-access", saved.AccessToken)
	}
	if saved.RefreshToken != "next-refresh" {
		t.Errorf("persisted refresh token = %q, want next-refresh", saved.RefreshToken)
	}
	if saved.Pending != nil {
		t.Error("successful refresh left a pending response")
	}
	if saved.Subject != "subject" || saved.ClientID != c.selected {
		t.Error("refresh changed registration identity")
	}

}

func TestRefreshWithNoReplacementTokenKeepsTheExistingGrant(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{
			"access_token": "refreshed-access", "token_type": "Bearer", "expires_in": 3600,
		})
	})
	if token, err := c.Token(t.Context()); err != nil || token != "refreshed-access" {
		t.Fatalf("refresh did not return the new access token: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil || saved.AccessToken != "refreshed-access" || saved.RefreshToken != "refresh" ||
		saved.Scopes != oauth.Scope || saved.Pending != nil {
		t.Fatalf("refresh did not persist the new access token and existing grant: %v", err)
	}
}

func TestCanceledRefreshSavesReplacementWithoutReturningToken(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{
			AccessToken: "replacement", RefreshToken: "rotated", TokenType: "Bearer", ExpiresIn: 3600,
		})
	})
	if token, err := c.Token(ctx); token != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh returned a token or the wrong error: %v", err)
	}
	if token, err := c.Token(t.Context()); err != nil || token != "replacement" || calls != 1 {
		t.Fatalf("replacement was not recovered without refreshing again: calls=%d err=%v", calls, err)
	}
}

func TestEarliestRefresh(t *testing.T) {
	c := testClient(t)
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	seed(t, c, now.Add(20*time.Second))
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	v.EarliestRefreshAt = now.Add(30 * time.Second).Unix()
	if err := c.saveCredential(&v); err != nil {
		t.Fatal(err)
	}
	if _, e := c.Token(context.Background()); e != nil {
		t.Fatal(e)
	}
	v.ExpiresAt = now.Add(-time.Second)
	if err := c.saveCredential(&v); err != nil {
		t.Fatal(err)
	}
	if _, e := c.Token(context.Background()); !errors.Is(e, ErrAuth) {
		t.Fatalf("want auth failure: %v", e)
	}
}

func TestTerminalRefreshClearsGrantAndKeepsRegistration(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(400, map[string]string{"error": "invalid_grant", "error_description": "secret should not appear"})
	})
	_, e := c.Token(context.Background())
	if !errors.Is(e, ErrAuth) || strings.Contains(e.Error(), "secret") || !strings.Contains(e.Error(), "ChatGPT Settings") {
		t.Fatal("missing recovery guidance or unsafe terminal error")
	}
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if v.RefreshToken != "" || v.Scopes != "" || !v.ExpiresAt.IsZero() || v.Subject != "subject" {
		t.Fatal("invalid grant retained credentials or lost its registration")
	}
	accounts, err := c.Status(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].SignedIn || accounts[0].PlanEnabled {
		t.Fatalf("status still reports a usable grant after invalid_grant: %v", err)
	}
}

func TestTerminalRefreshRevokesAnUnexpiredAccessToken(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(30*time.Second))
	expectRevocation(t, c, "access", "access_token", transportFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}))
	if token, err := c.Token(t.Context()); token != "" || !oauth.InvalidRefreshGrant(err) || !strings.Contains(err.Error(), "ChatGPT Settings") {
		t.Fatalf("missing cleanup guidance for the rejected grant: %v", err)
	}
}

func TestCanceledTerminalRefreshCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failWrite bool
	}{
		{name: "clears local grant"},
		{name: "reports a failed cleanup write", failWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				if tc.failWrite {
					c.testStoreFaults().write = errors.New("keyring write failed")
				}
				cancel()
				return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
			})
			_, err := c.Token(ctx)
			if !errors.Is(err, context.Canceled) || !oauth.InvalidRefreshGrant(err) {
				t.Fatalf("expected cancellation and the terminal provider error: %v", err)
			}
			if tc.failWrite {
				if !strings.Contains(err.Error(), "could not confirm local credential cleanup") {
					t.Fatalf("expected a credential cleanup error: %v", err)
				}
				return
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, saved)
		})
	}
}

func TestInterruptedRefreshPreservesGrantAndScope(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	key := testutil.NewRSAKey(t)
	var refreshes, fetches int
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case tokenURL:
			refreshes++
			return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: testutil.SignRS256(t, key, claims("")), TokenType: "Bearer", ExpiresIn: 3600})
		case jwksURL:
			fetches++
			if fetches == 1 {
				return nil, errors.New("temporary JWKS failure")
			}
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		default:
			t.Fatal("unexpected request")
			return nil, nil
		}
	})
	if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("pending tokens must not escape: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil || saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated-refresh" || saved.Pending.Response.Scope != nil || saved.AccessToken != "access" || saved.Scopes != oauth.Scope {
		t.Fatal("rotation not durably staged")
	}
	resumed := *c
	token, err := resumed.Token(context.Background())
	if err != nil || token != "rotated-access" || refreshes != 1 {
		t.Fatalf("did not resume validation: refreshes=%d err=%v", refreshes, err)
	}
	saved, err = c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" || saved.Scopes != oauth.Scope {
		t.Fatal("pending token not committed")
	}
}

// Check which credential was sent to revocation for this registration.
func expectRevocation(t *testing.T, c *Client, token, kind string, next http.RoundTripper) {
	t.Helper()
	calls := 0
	t.Cleanup(func() {
		want := 0
		if token != "" {
			want = 1
		}
		if calls != want {
			t.Errorf("remote revocations = %d, want %d", calls, want)
		}
	})
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case discoveryURL:
			return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
		case oauth.Issuer + "/revoke":
			calls++
			if r.FormValue("token") != token || r.FormValue("token_type_hint") != kind || r.FormValue("client_id") != c.selected {
				t.Error("revoked the wrong grant")
			}
			return jsonResponse(http.StatusOK, map[string]any{})
		default:
			return next.RoundTrip(r)
		}
	})
}

func TestRejectedCheckpointOnlyRetriesRevocation(t *testing.T) {
	for _, operation := range []string{"review", "reauthorize"} {
		t.Run(operation, func(t *testing.T) {
			const revokeURL = oauth.Issuer + "/revoke"
			c := testClient(t)
			seed(t, c, time.Now().Add(time.Hour))
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			saved.Pending = &pendingRefresh{RevokeOnly: true, Response: oauth.TokenResponse{RefreshToken: "replacement"}}
			if err := c.saveCredential(&saved); err != nil {
				t.Fatal(err)
			}
			var revoked bool
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == discoveryURL {
					return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": revokeURL})
				}
				if r.URL.String() != revokeURL || r.FormValue("token") != "replacement" {
					return nil, errors.New("unexpected request for rejected checkpoint")
				}
				revoked = true
				return jsonResponse(http.StatusOK, nil)
			})
			var token string
			if operation == "reauthorize" {
				err = c.SignIn(t.Context(), c.selected, func(string) error {
					t.Fatal("opened the browser before completing pending revocation")
					return nil
				})
			} else {
				token, err = c.Token(t.Context())
			}
			if token != "" || err == nil || !revoked {
				t.Fatalf("rejected replacement: revoked=%v err=%v", revoked, err)
			}
			saved, err = c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, saved)
		})
	}
}

func TestRefreshedWrongIdentityNeverExposed(t *testing.T) {
	for _, tt := range []struct {
		name, refreshToken string
		checkpointed       bool
		repeatsAccess      bool
	}{
		{"rotated checkpoint", "new-refresh", true, false},
		{"unchanged checkpoint", "refresh", true, false},
		{"omitted checkpoint", "", true, false},
		{"fresh reply without rotation", "", false, false},
		{"both tokens unchanged", "", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			cl := claims("")
			cl["sub"] = "different"
			response := oauth.TokenResponse{AccessToken: "wrong-account", RefreshToken: tt.refreshToken, IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}
			if tt.repeatsAccess {
				response.AccessToken = v.AccessToken
			}
			if tt.checkpointed {
				v.Pending = &pendingRefresh{Response: response, ReceivedAt: time.Now()}
				if err := c.saveCredential(&v); err != nil {
					t.Fatal(err)
				}
			}
			before, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			jwksCalls := 0
			replacement, kind := "wrong-account", "access_token"
			if tt.refreshToken != "" && tt.refreshToken != v.RefreshToken {
				replacement, kind = tt.refreshToken, "refresh_token"
			} else if tt.repeatsAccess {
				replacement = "" // Only the previous identity's credentials are known.
			}
			expectRevocation(t, c, replacement, kind, transportFunc(func(r *http.Request) (*http.Response, error) {
				if !tt.checkpointed && r.URL.String() == tokenURL {
					return jsonResponse(http.StatusOK, response)
				}
				if r.URL.String() != jwksURL {
					return nil, errors.New("unexpected request instead of JWKS validation")
				}
				jwksCalls++
				return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
			}))

			token, err := c.Token(t.Context())

			if token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "identity changed") {
				t.Fatalf("Token = %q, error = %v; want no token and identity-change authentication failure", token, err)
			}
			if jwksCalls != 1 {
				t.Errorf("JWKS calls = %d, want 1", jwksCalls)
			}
			after, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, after)
			if after.ClientID != before.ClientID || after.Subject != before.Subject {
				t.Error("rejection changed account identity")
			}
			if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrAuth) || jwksCalls != 1 {
				t.Fatalf("retry = %q, %v; JWKS calls = %d; want signed-out failure without network", token, err, jwksCalls)
			}
		})
	}
}

func TestPendingRefreshDelayedPastIDExpiry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		delay       time.Duration
		wantRefresh bool
	}{{"access still valid", 20 * time.Minute, false}, {"access expired", 2 * time.Hour, true}} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received.Add(tt.delay)
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(10 * time.Minute).Unix()
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case jwksURL:
					return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
				case tokenURL:
					refreshes++
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					if r.Form.Get("refresh_token") != "pending-refresh" {
						t.Error("reused consumed refresh token")
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600})
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantRefresh {
				if refreshes != 1 || token != "fresh-access" || saved.RefreshToken != "fresh-refresh" {
					t.Fatal("expired access was returned instead of refreshing replacement")
				}
			} else {
				if refreshes != 0 || token != "pending-access" || !saved.ExpiresAt.Equal(received.Add(time.Hour)) {
					t.Fatal("pending recovery extended actual access expiry")
				}
			}
			if saved.Pending != nil {
				t.Fatal("pending response was not committed")
			}
		})
	}
}

func TestMalformedRefreshTokenInSavedCheckpointKeepsPriorGrant(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	saved.Pending = &pendingRefresh{
		ReceivedAt: time.Now(),
		Response: oauth.TokenResponse{
			AccessToken: "replacement", RefreshToken: " ", TokenType: "Bearer", ExpiresIn: 3600,
		},
	}
	if err := c.saveCredential(&saved); err != nil {
		t.Fatal(err)
	}
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("saved malformed checkpoint was not rejected: %v", err)
	}
	current, err := c.loadCredential(c.selected)
	if err != nil || current.RefreshToken != saved.RefreshToken || current.AccessToken != saved.AccessToken || current.Pending != nil {
		t.Fatalf("saved malformed checkpoint disturbed the prior grant: %v", err)
	}
	if current.Generation != saved.Generation {
		t.Fatal("discarding the checkpoint changed the grant generation")
	}
}

func TestPendingRefreshAfterClockMovesBack(t *testing.T) {
	c := testClient(t)
	received := time.Now().UTC().Truncate(time.Second)
	now := received.Add(-time.Hour)
	c.now = func() time.Time { return now }
	seed(t, c, now.Add(-time.Minute))
	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	saved.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "new-access", RefreshToken: "rotated", TokenType: "Bearer", ExpiresIn: 3600}, ReceivedAt: received}
	if err := c.saveCredential(&saved); err != nil {
		t.Fatal(err)
	}
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected provider request while recovering the checkpoint")
		return nil, nil
	})
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("received %q, %v with a future receipt", token, err)
	}
	now = received
	if token, err := c.Token(t.Context()); token != "new-access" || err != nil {
		t.Fatalf("recovery returned %q, %v", token, err)
	}
}

func TestPendingRefreshRejectsPermanentFailures(t *testing.T) {
	for _, name := range []string{"zero receipt", "pre-epoch receipt", "expired at receipt", "malformed ID token", "invalid signature", "incomplete response"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			now := time.Now().UTC().Truncate(time.Second)
			c.now = func() time.Time { return now }
			seed(t, c, now.Add(-time.Minute))
			received := now.Add(-time.Hour)
			key := testutil.NewRSAKey(t)
			cl := claims("")
			cl["iat"] = received.Add(-time.Minute).Unix()
			cl["exp"] = received.Add(time.Minute).Unix()
			switch name {
			case "zero receipt":
				received = time.Time{}
			case "pre-epoch receipt":
				received = time.Unix(-1, 0)
			case "expired at receipt":
				cl["exp"] = received.Unix()
			}
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{Response: oauth.TokenResponse{AccessToken: "pending-access", RefreshToken: "pending-refresh", IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", Scope: scopeValue(oauth.Scope), ExpiresIn: 3600}, ReceivedAt: received}
			switch name {
			case "malformed ID token":
				v.Pending.Response.IDToken = "malformed"
			case "invalid signature":
				v.Pending.Response.IDToken = testutil.SignRS256(t, testutil.NewRSAKey(t), cl)
			case "incomplete response":
				v.Pending.Response.TokenType = "unsupported"
			}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			expectRevocation(t, c, "pending-refresh", "refresh_token", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Fatal("invalid pending record caused refresh")
				}
				return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
			}))
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid receipt/identity accepted: %v", err)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, saved)
		})
	}
}

func TestRefreshRechecksExpiryAfterValidation(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second, 2 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c := testClient(t)
			received := time.Now().UTC().Truncate(time.Second)
			now := received
			c.now = func() time.Time { return now }
			seed(t, c, received.Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			cl := claims("")
			cl["iat"] = received.Unix()
			cl["exp"] = received.Add(time.Hour).Unix()
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					want := "refresh"
					if refreshes > 1 {
						want = "rotated-refresh"
					}
					if r.Form.Get("refresh_token") != want {
						t.Fatal("reused consumed refresh token")
					}
					if refreshes == 1 {
						return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "short-access", RefreshToken: "rotated-refresh", IDToken: testutil.SignRS256(t, key, cl), TokenType: "Bearer", ExpiresIn: 1, Scope: scopeValue(oauth.Scope)})
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scope: scopeValue(oauth.Scope)})
				case jwksURL:
					now = received.Add(delay)
					return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			token, err := c.Token(context.Background())
			if delay == 0 {
				if err != nil || token != "short-access" {
					t.Fatalf("valid token rejected: %v", err)
				}
				return
			}
			if token != "" || !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) {
				t.Fatalf("expired refresh must return a retryable error and no token: %v", err)
			}
			if refreshes != 1 {
				t.Fatal("retried refresh within the same call")
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Pending != nil || saved.RefreshToken != "rotated-refresh" || !saved.ExpiresAt.Equal(received.Add(time.Second)) {
				t.Fatal("rotated credentials were not preserved with their original expiry")
			}
			token, err = c.Token(context.Background())
			if err != nil || token != "fresh-access" || refreshes != 2 {
				t.Fatalf("next call failed to recover using rotated credentials: %v", err)
			}
		})
	}
}

func assertClearedCredentials(t *testing.T, saved credential) {
	t.Helper()
	if saved.Pending != nil || saved.AccessToken != "" || saved.RefreshToken != "" || saved.IDToken != "" || saved.Scopes != "" || !saved.ExpiresAt.IsZero() || saved.EarliestRefreshAt != 0 {
		t.Error("rejected credentials or refresh metadata remain stored")
	}
}

func TestRefreshFailurePreservesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"transport failure", 0, "", ErrNetwork},
		{"request timeout", http.StatusRequestTimeout, "invalid_grant", ErrNetwork},
		{"rate limit", http.StatusTooManyRequests, "invalid_grant", ErrNetwork},
		{"server failure", http.StatusInternalServerError, "invalid_grant", ErrNetwork},
		{"service unavailable", http.StatusServiceUnavailable, "invalid_grant", ErrNetwork},
		{"bare unauthorized", http.StatusUnauthorized, "unknown", ErrAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			refreshes := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				refreshes++
				if r.Method != http.MethodPost || r.URL.String() != tokenURL {
					t.Errorf("unexpected refresh request: %s %s", r.Method, r.URL)
				}
				if tc.status == 0 {
					return nil, errors.New("secret in transport error")
				}
				return jsonResponse(tc.status, map[string]string{"error": tc.code})
			})
			token, err := c.Token(t.Context())
			if token != "" || !errors.Is(err, tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("expected a safe %v without a returned token, got %v", tc.want, err)
			}
			if refreshes != 1 {
				t.Fatalf("refresh requests = %d, want 1", refreshes)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil || saved.AccessToken != "access" || saved.RefreshToken != "refresh" || saved.Pending != nil {
				t.Fatalf("refresh failure changed the saved grant: %v", err)
			}
		})
	}
}

func TestMalformedSuccessfulRefreshWithoutRotationPreservesGrant(t *testing.T) {
	for _, body := range []string{
		`null`,
		`{}`,
		`{"expires_in":3600}`,
		`{"access_token":"replacement","token_type":"Bearer"}`,
		`{"access_token":"replacement","refresh_token":`,
		`{"access_token":"invalid access","token_type":"Bearer","expires_in":3600}`,
		`{"access_token":"replacement","id_token":"invalid","token_type":"Bearer","expires_in":3600}`,
		`{"access_token":"replacement","refresh_token":"refresh","token_type":"Bearer","expires_in":-1}`,
	} {
		c := testClient(t)
		seed(t, c, time.Now().Add(-time.Minute))
		previous, err := c.loadCredential(c.selected)
		if err != nil {
			t.Fatal(err)
		}
		c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if _, err := c.Token(t.Context()); !errors.Is(err, ErrNetwork) {
			t.Fatalf("malformed successful refresh should be retryable: %v", err)
		}
		saved, err := c.loadCredential(c.selected)
		if err != nil || saved.AccessToken != "access" || saved.RefreshToken != "refresh" || saved.Pending != nil || saved.Generation != previous.Generation {
			t.Fatal("malformed successful response erased credentials")
		}
	}
}

func TestIdentityOnlyRefreshKeepsThePriorGrant(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	previous, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	key := testutil.NewRSAKey(t)
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == jwksURL {
			return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
		}
		return jsonResponse(http.StatusOK, oauth.TokenResponse{
			IDToken: testutil.SignRS256(t, key, claims("")), Scope: scopeValue("openid"),
		})
	})
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrNetwork) {
		t.Fatalf("identity-only refresh was not rejected as retryable: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil || saved.AccessToken != "access" || saved.RefreshToken != "refresh" || saved.Scopes != oauth.Scope || saved.Pending != nil || saved.Generation != previous.Generation {
		t.Fatalf("identity-only refresh changed the prior grant: %v", err)
	}
}

func TestPartiallyDecodedRefreshCannotReuseRotatedGrant(t *testing.T) {
	for _, tc := range []struct {
		tokenStatus  int
		body         string
		revokeStatus int
	}{
		{http.StatusOK, `{"refresh_token":"rotated","expires_in":"invalid"}`, http.StatusOK},
		{http.StatusOK, `{"refresh_token":"rotated","refresh_token":"bad\ntoken"}`, http.StatusOK},
		{http.StatusOK, `{"refresh_token":"rotated","expires_in":`, http.StatusOK},
		{http.StatusOK, `{"refresh_token":"rotated","expires_in":`, http.StatusForbidden},
		{http.StatusOK, `{"refresh_token":"rotated"} trailing data`, http.StatusOK},
		{http.StatusOK, `{"access_token":"replacement","refresh_token":"rotated","refresh_token":"","token_type":"Bearer","expires_in":3600}`, http.StatusOK},
		{http.StatusOK, `{"access_token":"replacement","refresh_token":"rotated","refreſh_token":"","token_type":"Bearer","expires_in":3600}`, http.StatusOK},
		{http.StatusOK, `{"access_token":"replacement","refresh_token":"rotated","scope":"scope-one","SCOPE":"scope-two","token_type":"Bearer","expires_in":3600}`, http.StatusOK},
		{http.StatusCreated, `{"refresh_token":"rotated"}`, http.StatusOK},
	} {
		c := testClient(t)
		seed(t, c, time.Now().Add(-time.Minute))
		revocations := 0
		c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.String() {
			case tokenURL:
				return &http.Response{StatusCode: tc.tokenStatus, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			case discoveryURL:
				return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
			case oauth.Issuer + "/revoke":
				revocations++
				saved, err := c.loadCredential(c.selected)
				if err != nil || saved.signedIn() || saved.planEnabled() || saved.Pending == nil || !saved.Pending.RevokeOnly || saved.Pending.Response.RefreshToken != "rotated" {
					t.Fatalf("rejected grant still usable or missing its cleanup token: %v", err)
				}
				if r.FormValue("token") != "rotated" || r.FormValue("token_type_hint") != "refresh_token" || r.FormValue("client_id") != c.selected {
					t.Fatal("revocation did not identify the discarded replacement")
				}
				return jsonResponse(tc.revokeStatus, nil)
			}
			return nil, errors.New("unexpected request")
		})
		token, err := c.Token(t.Context())
		if token != "" || !errors.Is(err, ErrAuth) || revocations != 1 {
			t.Fatalf("malformed rotated response was not rejected: %v", err)
		}
		if !strings.Contains(err.Error(), "ChatGPT Settings") {
			t.Fatalf("missing remote recovery guidance for the malformed reply: %v", err)
		}
		if strings.Contains(err.Error(), "Disconnect Roast in ChatGPT Settings") != (tc.revokeStatus != http.StatusOK) {
			t.Fatalf("wrong revocation guidance: %v", err)
		}
		saved, err := c.loadCredential(c.selected)
		if err != nil {
			t.Fatal(err)
		}
		assertClearedCredentials(t, saved)
	}
}

func TestConflictingRefreshTokensClearTheConsumedGrant(t *testing.T) {
	for _, access := range []string{"", "access", "unique"} {
		t.Run("access-"+access, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			revoked := false
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					body := `{"access_token":"` + access + `","refresh_token":"first","refresh_token":"second"}`
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
				case discoveryURL:
					return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
				case oauth.Issuer + "/revoke":
					want := access
					if want == "" {
						want = "access" // The saved access token is still known.
					}
					if r.FormValue("token") != want || r.FormValue("token_type_hint") != "access_token" {
						t.Fatal("attempted to revoke an arbitrary token")
					}
					saved, err := c.loadCredential(c.selected)
					if err != nil || saved.Pending == nil || !saved.Pending.UnknownRefresh {
						t.Fatalf("lost the manual recovery requirement before revocation: %v", err)
					}
					revoked = true
					return jsonResponse(http.StatusOK, nil)
				default:
					return nil, errors.New("unexpected request")
				}
			})
			token, err := c.Token(t.Context())
			if token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "ChatGPT Settings") || !revoked {
				t.Fatalf("ambiguous renewal did not explain manual recovery: %v", err)
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			assertClearedCredentials(t, saved)
		})
	}
}

func TestMalformedRotationAttemptsRevocationWhenStorageFails(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	expectRevocation(t, c, "replacement", "refresh_token", transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != tokenURL {
			t.Fatalf("unexpected request to %s", r.URL.Host)
		}
		c.testStoreFaults().write = errors.New("keyring write failed")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"refresh_token":"replacement","expires_in":`))}, nil
	}))
	token, err := c.Token(t.Context())
	if token != "" || !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "repair credential storage") ||
		strings.Contains(err.Error(), "remote revocation was not confirmed") {
		t.Fatalf("storage failure lost the revocation result: %v", err)
	}
}

func TestMalformedOAuthErrorDoesNotEraseCredentials(t *testing.T) {
	for _, raw := range []string{
		`{"error":"server_error","error":"invalid_grant"}`,
		`{"error":"server_error","Error":"invalid_grant"}`,
		`{"error":{"code":"server_error","code":"invalid_grant"}}`,
		`{"error":{"code":"invalid_grant","code":42}}`,
		`{"error":{"code":"invalid_grant","code":null}}`,
		`{"error":"invalid_grant","extra":`,
		`{"error":"invalid_grant"} trailing data`,
		`{"error":"invalid_grant"}{}`,
		`{"error":"invalid_grant"}` + strings.Repeat(" ", 1<<20),
	} {
		c := testClient(t)
		seed(t, c, time.Now().Add(-time.Minute))
		c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
		})
		if _, err := c.Token(context.Background()); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "oauth_error") {
			t.Fatalf("malformed error retained a terminal category: %v", err)
		}
		v, err := c.loadCredential(c.selected)
		if err != nil || v.AccessToken != "access" || v.RefreshToken != "refresh" {
			t.Fatal("malformed error erased credentials")
		}
	}
}

func scopeValue(scope string) *string { return &scope }

func TestRefreshScopePresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   any
		present bool
		want    string
	}{
		{"omitted", nil, false, oauth.Scope},
		{"null", nil, true, oauth.Scope},
		{"unchanged", oauth.Scope, true, oauth.Scope},
		{"reduced", "openid profile email", true, "openid profile email"},
		{"empty", "", true, ""},
		{"whitespace", " \t ", true, " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			var calls int
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != tokenURL {
					t.Fatal("unexpected request")
				}
				data := map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "token_type": "Bearer", "expires_in": 3600}
				if tc.present {
					data["scope"] = tc.scope
				}
				return jsonResponse(http.StatusOK, data)
			})
			token, err := c.Token(context.Background())
			if tc.want == oauth.Scope {
				if err != nil || token != "rotated-access" {
					t.Fatalf("unchanged grant lost: %v", err)
				}
			} else if !errors.Is(err, ErrAuth) || token != "" || !strings.Contains(err.Error(), "permission was removed") {
				t.Fatalf("removed grant accepted: %v", err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.Scopes != tc.want || v.RefreshToken != "rotated-refresh" || v.Pending != nil || calls != 1 {
				t.Fatalf("refresh result not committed correctly: %v", err)
			}
		})
	}
}

func TestInitialTokenDoesNotInheritScopes(t *testing.T) {
	c := testClient(t)
	v := credential{Scopes: oauth.Scope}
	if err := v.applyTokenResponse(oauth.TokenResponse{IDToken: "identity-only"}, c.now()); err != nil || v.Scopes != "" {
		t.Fatalf("initial authorization inherited an old grant: %v", err)
	}
}

func TestRefreshMetadataDoesNotDiscardRotatedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint any
	}{
		{"RFC3339", "2026-11-09T00:00:00Z"},
		{"numeric string", "1794182400"},
		{"invalid date", "not-a-date"},
		{"unexpected object", map[string]any{"unexpected": true}},
		{"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			var refreshes, fetches int
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					return jsonResponse(http.StatusOK, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "id_token": testutil.SignRS256(t, key, claims("")), "token_type": "Bearer", "expires_in": 3600, "earliest_refresh_at": tc.hint})
				case jwksURL:
					fetches++
					if fetches == 1 {
						return nil, errors.New("temporary JWKS failure")
					}
					return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
				default:
					t.Fatal("unexpected request")
					return nil, nil
				}
			})
			if token, err := c.Token(context.Background()); token != "" || !errors.Is(err, ErrNetwork) {
				t.Fatalf("pending credentials escaped or response lost: %v", err)
			}
			v, err := c.loadCredential(c.selected)
			if err != nil || v.Pending == nil || v.Pending.Response.RefreshToken != "rotated-refresh" || v.AccessToken != "access" {
				t.Fatal("rotated credentials were not checkpointed")
			}
			resumed := *c
			if token, err := resumed.Token(context.Background()); err != nil || token != "rotated-access" || refreshes != 1 {
				t.Fatalf("could not recover rotated credentials: %v", err)
			}
			v, err = c.loadCredential(c.selected)
			if err != nil || v.Pending != nil || v.RefreshToken != "rotated-refresh" || v.Scopes != oauth.Scope {
				t.Fatal("rotated credentials were not committed")
			}
		})
	}
}

func TestPendingRefreshRetainsCheckpointWhenKeysUnavailable(t *testing.T) {
	for _, name := range []string{"HTTP failure", "missing signing key"} {
		t.Run(name, func(t *testing.T) {
			c := testClient(t)
			now := time.Now().UTC().Truncate(time.Second)
			c.now = func() time.Time { return now }
			seed(t, c, now.Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			v, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			v.Pending = &pendingRefresh{
				Response: oauth.TokenResponse{
					AccessToken: "replacement", RefreshToken: "rotated",
					IDToken: testutil.SignRS256(t, key, claims("")), TokenType: "Bearer",
					ExpiresIn: 3600, Scope: scopeValue(oauth.Scope),
				},
				ReceivedAt: now,
			}
			if err := c.saveCredential(&v); err != nil {
				t.Fatal(err)
			}
			before, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != jwksURL {
					t.Error("pending recovery attempted another refresh")
				}
				if name == "HTTP failure" {
					return jsonResponse(http.StatusForbidden, map[string]string{"error": "unavailable"})
				}
				return jsonResponse(http.StatusOK, map[string]any{"keys": []any{}})
			})

			token, err := c.Token(t.Context())

			if token != "" || !errors.Is(err, ErrNetwork) || errors.Is(err, ErrAuth) || calls != 1 {
				t.Fatalf("Token = %q, %v; requests = %d; want retryable key failure", token, err, calls)
			}
			after, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Error("temporary key failure changed the durable checkpoint")
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Error("recovery replayed the consumed refresh token")
				}
				return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
			})
			if token, err := c.Token(t.Context()); err != nil || token != "replacement" {
				t.Fatalf("recovery = %q, %v", token, err)
			}
		})
	}
}

func TestRefreshPersistenceFailureProvidesRecoveryGuidance(t *testing.T) {
	for _, stage := range []string{"checkpoint", "checkpoint without rotation", "validated credentials"} {
		t.Run(stage, func(t *testing.T) {
			checkpoint := strings.HasPrefix(stage, "checkpoint")
			c := testClient(t)
			seed(t, c, time.Now().Add(-time.Minute))
			key := testutil.NewRSAKey(t)
			faults := c.testStoreFaults()
			blockWrite := func() {
				faults.write = errors.New("keyring write failed")
			}
			refreshes := 0
			replacement := ""
			if stage == "checkpoint" {
				replacement = "rotated"
			}
			expectRevocation(t, c, replacement, "refresh_token", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.String() {
				case tokenURL:
					refreshes++
					if checkpoint {
						blockWrite()
					}
					refreshToken := "rotated"
					if stage == "checkpoint without rotation" {
						refreshToken = "refresh"
					}
					return jsonResponse(http.StatusOK, oauth.TokenResponse{
						AccessToken: "replacement", RefreshToken: refreshToken,
						IDToken: testutil.SignRS256(t, key, claims("")), TokenType: "Bearer",
						ExpiresIn: 3600, Scope: scopeValue(oauth.Scope),
					})
				case jwksURL:
					if stage == "validated credentials" {
						blockWrite()
					}
					return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
					return nil, nil
				}
			}))

			token, err := c.Token(t.Context())

			if token != "" || err == nil || !strings.Contains(strings.ToLower(err.Error()), "repair credential storage") {
				t.Fatalf("Token = %q, %v; want storage recovery guidance without a token", token, err)
			}
			if stage == "checkpoint" && !strings.Contains(err.Error(), "could not confirm local credential cleanup") {
				t.Fatalf("failure incorrectly implies durable cleanup: %v", err)
			}
			if stage == "checkpoint without rotation" && strings.Contains(err.Error(), "credential cleanup") {
				t.Fatalf("unchanged refresh token should not be cleared: %v", err)
			}
			faults.write = nil
			saved, err := c.loadCredential(c.selected)
			if err != nil {
				t.Fatal(err)
			}
			if checkpoint {
				if saved.RefreshToken != "refresh" || saved.Pending != nil {
					t.Error("unexpected state after failed checkpoint and cleanup")
				}
				return
			}
			if saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated" {
				t.Fatal("failed final write lost the durable replacement")
			}
			c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != jwksURL {
					t.Error("recovery replayed a refresh")
				}
				return jsonResponse(http.StatusOK, testutil.RS256KeySet(key))
			})
			if token, err := c.Token(t.Context()); err != nil || token != "replacement" || refreshes != 1 {
				t.Fatalf("recovery = %q, %v; refreshes = %d", token, err, refreshes)
			}
		})
	}
}

func TestFailedCheckpointKeepsPublishedReplacement(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(-time.Minute))
	writeErr := errors.New("could not confirm keyring commit")
	err := c.withStore(t.Context(), func(accounts *accountStore) error {
		saved, err := accounts.credential(c.selected)
		if err != nil {
			return err
		}
		previousGeneration := saved.Generation
		saved.Pending = &pendingRefresh{
			Response:   oauth.TokenResponse{AccessToken: "replacement", RefreshToken: "rotated"},
			ReceivedAt: time.Now(),
		}
		if err := accounts.saveCredential(&saved); err != nil {
			return err
		}
		return c.refreshCheckpointError(t.Context(), accounts, &saved, previousGeneration, errors.Join(storage.ErrPublished, writeErr))
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("lost checkpoint write error: %v", err)
	}
	saved, err := c.loadCredential(c.selected)
	if err != nil || saved.Pending == nil || saved.Pending.Response.RefreshToken != "rotated" {
		t.Fatalf("published rotation was lost after the write error: %v", err)
	}
}
