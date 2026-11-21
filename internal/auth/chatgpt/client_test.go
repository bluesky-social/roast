//go:build darwin || linux

package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

func TestNewDoesNotCreateCredentials(t *testing.T) {
	mockKeyring(t)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Token(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatal("missing login not classified")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "roast", "chatgpt-plan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only action created the keyring lock directory")
	}
}

func mockKeyring(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	keyring.MockInit()
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonResponse returns encoding failures through the mock transport, including
// when the request runs on a sign-in goroutine.
func jsonResponse(status int, value any) (*http.Response, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
}

func testClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	store, err := storage.OpenWithKeyring(&testKeyring{items: make(map[string]string)}, dir)
	if err != nil {
		t.Fatal(err)
	}
	provider := oauth.NewClient()
	provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("unexpected network request")
		return nil, errors.New("unexpected")
	})
	return &Client{store: testAccountStore{Store: store, faults: &storeFaults{}}, now: time.Now, provider: provider}
}

type testKeyring struct {
	mu    sync.Mutex
	items map[string]string
}

func (k *testKeyring) Get(_, item string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if value, ok := k.items[item]; ok {
		return value, nil
	}
	return "", keyring.ErrNotFound
}

func (k *testKeyring) Set(_, item, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[item] = value
	return nil
}

func (k *testKeyring) Delete(_, item string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.items[item]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.items, item)
	return nil
}

type storeFaults struct {
	write, afterWrite error
}

type testAccountStore struct {
	storage.Store
	faults *storeFaults
}

func (s testAccountStore) WriteJSON(ctx context.Context, value any) error {
	if s.faults.write != nil {
		return s.faults.write
	}
	if err := s.Store.WriteJSON(ctx, value); err != nil {
		return err
	}
	return s.faults.afterWrite
}

func (s testAccountStore) WithLock(ctx context.Context, fn func(storage.Store) error) error {
	return s.Store.WithLock(ctx, func(locked storage.Store) error {
		return fn(testAccountStore{Store: locked, faults: s.faults})
	})
}

func (c *Client) testStoreFaults() *storeFaults {
	return c.store.(testAccountStore).faults
}

func seed(t *testing.T, c *Client, exp time.Time) {
	t.Helper()
	c.selected = "oaiapp_test"
	if err := c.withStore(t.Context(), func(accounts *accountStore) error {
		accounts.HostID, accounts.Active = "host", c.selected
		saved := credential{Account: Account{ClientID: c.selected, Subject: "subject", ExpiresAt: exp}, AccessToken: "access", RefreshToken: "refresh", Scopes: oauth.Scope}
		return accounts.saveCredential(&saved)
	}); err != nil {
		t.Fatal(err)
	}
}

func (c *Client) loadCredential(id string) (credential, error) {
	accounts, err := c.loadStore()
	if err != nil {
		return credential{}, err
	}
	return accounts.credential(id)
}

func (c *Client) saveCredential(saved *credential) error {
	return c.withStore(context.Background(), func(accounts *accountStore) error {
		return accounts.saveCredential(saved)
	})
}

const (
	tokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	jwksURL      = "https://auth.openai.com/.well-known/jwks.json"
	discoveryURL = "https://auth.openai.com/.well-known/openid-configuration"
)

func claims(nonce string) map[string]any {
	return map[string]any{"iss": oauth.Issuer, "sub": "subject", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
}

func TestSignOutKeepsRegistrationOnNetworkFailure(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	c.provider.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	e := c.SignOut(context.Background(), "")
	if e == nil || !strings.Contains(e.Error(), "remote revocation was not confirmed") {
		t.Fatal("must explain unconfirmed revocation")
	}
	v, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	if v.AccessToken != "" || v.RefreshToken != "" || v.Subject != "subject" {
		t.Fatal("wrong signout state")
	}
}

func TestSignOutRevokesAccessOnlyGrant(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	saved.RefreshToken, saved.Scopes = "", "openid resource.invoke"
	saved.Pending = &pendingRefresh{Response: oauth.TokenResponse{RefreshToken: " ", AccessToken: "invalid access"}}
	if err := c.saveCredential(&saved); err != nil {
		t.Fatal(err)
	}
	revoked := false
	c.provider.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case discoveryURL:
			return jsonResponse(http.StatusOK, map[string]string{"issuer": oauth.Issuer, "revocation_endpoint": oauth.Issuer + "/revoke"})
		case oauth.Issuer + "/revoke":
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			saved, err := c.loadCredential(c.selected)
			if err != nil || saved.signedIn() || saved.Pending == nil || !saved.Pending.RevokeOnly || saved.Pending.Response.AccessToken != "access" {
				t.Fatalf("access-only sign-out did not checkpoint its token: %v", err)
			}
			revoked = r.Form.Get("token") == "access" && r.Form.Get("token_type_hint") == "access_token"
			return jsonResponse(http.StatusOK, nil)
		default:
			return nil, errors.New("unexpected revocation request")
		}
	})
	if err := c.SignOut(t.Context(), ""); err != nil || !revoked {
		t.Fatalf("sign-out did not revoke the access token: %v", err)
	}
	saved, err = c.loadCredential(c.selected)
	if err != nil || saved.signedIn() {
		t.Fatalf("sign-out did not clear the local grant: %v", err)
	}
}

func TestSignOutResumesAfterPublishedCheckpointError(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	faults := c.testStoreFaults()
	faults.afterWrite = storage.ErrPublished
	if err := c.SignOut(t.Context(), ""); !errors.Is(err, storage.ErrPublished) {
		t.Fatalf("unconfirmed sign-out checkpoint = %v", err)
	}
	faults.afterWrite = nil
	accounts, err := c.loadStore()
	if err != nil || accounts.Active != c.selected {
		t.Fatalf("interrupted sign-out lost the active registration: %v", err)
	}
	c.selected = accounts.Active
	c.provider.HTTP.Transport = mockSignInTransport(t, nil)
	if err := c.SignOut(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	accounts, err = c.loadStore()
	if err != nil || accounts.Active != "" || accounts.Accounts[c.selected].signedIn() {
		t.Fatalf("sign-out retry did not clear the saved grant: %v", err)
	}
}

func TestSelectedRegistrationPinnedAndIndependent(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	other := credential{Account: Account{ClientID: "oaiapp_other", Subject: "another", ExpiresAt: time.Now().Add(time.Hour)}, AccessToken: "other-access", RefreshToken: "other-refresh", Scopes: oauth.Scope}
	if err := c.saveCredential(&other); err != nil {
		t.Fatal(err)
	}
	if err := c.Select(context.Background(), other.ClientID); err != nil {
		t.Fatal(err)
	}
	token, err := c.Token(context.Background())
	if err != nil || token != "access" {
		t.Fatal("account switch changed in-flight client")
	}
	accounts, err := c.Status(context.Background())
	if err != nil || len(accounts) != 2 {
		t.Fatalf("status did not list both accounts: %v", err)
	}
	for _, account := range accounts {
		if account.Active != (account.ClientID == other.ClientID) || !account.PlanEnabled {
			t.Fatal("status did not reflect selected account")
		}
	}
}

func TestSignOutKeepsItsSelectedAccount(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	addMockAccount(t, c, "account-b")
	c.provider.HTTP.Transport = mockSignInTransport(t, nil)
	if err := c.Select(t.Context(), "account-b"); err != nil {
		t.Fatal(err)
	}
	if err := c.SignOut(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	accounts, err := c.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if accounts.Active != "account-b" || !accounts.Accounts["account-b"].signedIn() || accounts.Accounts[c.selected].signedIn() {
		t.Fatal("sign-out did not clear the pinned account and preserve the newly active account")
	}
}

func TestPendingRegistrationCannotSupplyAPlanToken(t *testing.T) {
	c := testClient(t)
	seed(t, c, time.Now().Add(time.Hour))
	saved, err := c.loadCredential(c.selected)
	if err != nil {
		t.Fatal(err)
	}
	saved.PendingRegistration = true
	if err := c.saveCredential(&saved); err != nil {
		t.Fatal(err)
	}
	if token, err := c.Token(t.Context()); token != "" || !errors.Is(err, ErrAuth) {
		t.Fatalf("invalid pending registration returned %q, %v", token, err)
	}
}
