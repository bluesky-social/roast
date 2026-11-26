package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/testutil"
)

func TestIDTokenValidation(t *testing.T) {
	now := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	key := testutil.NewRSAKey(t)
	otherKey := testutil.NewRSAKey(t)
	tests := []struct {
		name     string
		mutate   func(map[string]any)
		nonce    string
		wrongKey bool
		wantErr  bool
	}{
		{name: "valid", nonce: "nonce"},
		{name: "case-variant extensions", nonce: "nonce", mutate: func(v map[string]any) {
			v["AUD"] = 42
			v["SUB"] = []string{"extension"}
		}},
		{name: "wrong issuer", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["iss"] = "https://evil.invalid" }},
		{name: "uppercase audience field without exact claim", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) {
			v["AUD"] = v["aud"]
			delete(v, "aud")
		}},
		{name: "wrong audience", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["aud"] = "other" }},
		{name: "wrong authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["azp"] = "other" }},
		{name: "expired", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["exp"] = now.Add(-time.Minute).Unix() }},
		{name: "at expiry", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["exp"] = now.Unix() }},
		{name: "issued beyond allowed skew", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["iat"] = now.Unix() + 6 }},
		{name: "not before beyond allowed skew", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["nbf"] = now.Unix() + 6 }},
		{name: "matching authorized party", nonce: "nonce", mutate: func(v map[string]any) { v["azp"] = "oaiapp_test" }},
		{name: "null authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["azp"] = nil }},
		{name: "empty authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["azp"] = "" }},
		{name: "not yet valid", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["nbf"] = now.Add(time.Hour).Unix() }},
		{name: "wrong nonce", nonce: "wrong", wantErr: true},
		{name: "missing subject", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["sub"] = "" }},
		{name: "multiple audiences without authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) { v["aud"] = []string{"oaiapp_test", "other"} }},
		{name: "untrusted additional audience with authorized party", nonce: "nonce", wantErr: true, mutate: func(v map[string]any) {
			v["aud"] = []string{"oaiapp_test", "other"}
			v["azp"] = "oaiapp_test"
		}},
		{name: "single audience array", nonce: "nonce", mutate: func(v map[string]any) { v["aud"] = []string{"oaiapp_test"} }},
		{name: "wrong signature", nonce: "nonce", wrongKey: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				const wantURL = "https://auth.openai.com/.well-known/jwks.json"
				if r.URL.String() != wantURL {
					t.Errorf("JWKS URL = %q, want %q", r.URL, wantURL)
				}
				body, err := json.Marshal(testutil.RS256KeySet(key))
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			payload := idTokenClaims(now)
			if tt.mutate != nil {
				tt.mutate(payload)
			}
			signing := key
			if tt.wrongKey {
				signing = otherKey
			}
			token := testutil.SignRS256(t, signing, payload)

			identity, err := c.ValidateID(t.Context(), token, "oaiapp_test", tt.nonce, func() time.Time { return now })

			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateID error = %v, want error = %t", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidID) {
				t.Fatalf("rejected token must fail validation permanently, got %v", err)
			}
			if !tt.wantErr && identity.Subject != "subject" {
				t.Errorf("validated subject = %q, want subject", identity.Subject)
			}
		})
	}
}

func TestCanceledVerificationDoesNotRejectGrant(t *testing.T) {
	key := testutil.NewRSAKey(t)
	raw := testutil.SignRS256(t, testutil.NewRSAKey(t), idTokenClaims(time.Now()))
	keys, err := json.Marshal(testutil.RS256KeySet(key))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := NewClient()
	c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(keys))}, nil
	})
	if _, err := c.ValidateID(ctx, raw, "oaiapp_test", "nonce", time.Now); !errors.Is(err, context.Canceled) {
		t.Fatalf("verification returned %v, want cancellation", err)
	}
}

func TestIDTokenRejectsMalformedTokensBeforeFetchingKeys(t *testing.T) {
	payload, err := json.Marshal(idTokenClaims(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	const header = `{"alg":"RS256","kid":"unknown"}`
	claims := string(payload)
	for _, tc := range []struct{ name, header, payload, signature string }{
		{"unsigned", `{"alg":"none","kid":"test"}`, claims, ""},
		{"unencoded", `{"alg":"RS256","kid":"test","b64":false}`, claims, "signature"},
		{"incomplete payload", header, "{", "signature"},
		{"null payload", header, "null", "signature"},
		{"wrong audience type", header, `{"sub":"subject","aud":42}`, "signature"},
		{"missing signature", header, claims, ""},
		{"duplicate subject", header, strings.TrimSuffix(claims, "}") + `,"sub":null}`, "signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				t.Error("requested signing keys for a malformed token")
				return nil, errors.New("unexpected key request")
			})
			encode := base64.RawURLEncoding.EncodeToString
			token := encode([]byte(tc.header)) + "." + encode([]byte(tc.payload)) + "." + encode([]byte(tc.signature))
			if _, err := c.ValidateID(t.Context(), token, "oaiapp_test", "nonce", time.Now); !errors.Is(err, ErrInvalidID) {
				t.Fatalf("ValidateID returned %v, want ErrInvalidID", err)
			}
		})
	}
}

func TestSigningKeySetSkipsIneligibleRecords(t *testing.T) {
	key := testutil.NewRSAKey(t)
	now := time.Now()
	token := testutil.SignRS256(t, key, idTokenClaims(now))
	valid, err := json.Marshal(testutil.RS256KeySet(key))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, field, substitute string }{
		{"KEYS", `"keys"`, `"KEYS"`},
		{"KID", `"kid"`, `"KID"`},
		{"overlong exponent", `"e":"AQAB"`, `"e":"AAABAAE"`},
		{"disallowed key operation", `"kty":"RSA"`, `"kty":"RSA","key_ops":["encrypt"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Replace(valid, []byte(tc.field), []byte(tc.substitute), 1)
			if bytes.Equal(valid, body) {
				t.Fatal("fixture did not contain the expected field")
			}
			c := NewClient()
			c.HTTP.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})
			_, err := c.ValidateID(t.Context(), token, "oaiapp_test", "nonce", func() time.Time { return now })
			if !errors.Is(err, ErrNetwork) {
				t.Fatalf("ineligible signing key was treated as a usable key: %v", err)
			}
		})
	}
}

func idTokenClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":   Issuer,
		"sub":   "subject",
		"aud":   "oaiapp_test",
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": "nonce",
	}
}
