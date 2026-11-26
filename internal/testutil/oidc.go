package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// NewRSAKey generates an RSA key for a test token fixture.
func NewRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// SignRS256 signs claims for a test token with the fixture key ID.
func SignRS256(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	options := new(jose.SignerOptions).WithHeader("kid", "test")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, options)
	if err != nil {
		t.Errorf("create fixture signer: %v", err)
		return ""
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Errorf("sign fixture token: %v", err)
	}
	return token
}

// RS256KeySet returns the public key for tokens signed by SignRS256.
func RS256KeySet(key *rsa.PrivateKey) jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &key.PublicKey, KeyID: "test", Algorithm: string(jose.RS256), Use: "sig",
	}}}
}
