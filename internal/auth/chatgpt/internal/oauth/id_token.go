package oauth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/json"
	"github.com/go-jose/go-jose/v4/jwt"
)

type identityClaims struct {
	Subject         string          `json:"sub"`
	Email           string          `json:"email"`
	Issuer          string          `json:"iss"`
	Audience        jwt.Audience    `json:"aud"`
	AuthorizedParty json.RawMessage `json:"azp"`
	Nonce           string          `json:"nonce"`
	Exp             int64           `json:"exp"`
	Iat             int64           `json:"iat"`
	Nbf             int64           `json:"nbf"`
}

type Identity struct {
	Subject string
	Email   string
}

// ErrInvalidID identifies a rejected token, not a failure to retrieve its keys.
var ErrInvalidID = errors.New("ChatGPT ID token validation failed")

// ValidateID verifies a compact RS256 ID token for exactly one registration.
func (c Client) ValidateID(ctx context.Context, raw, clientID, nonce string, now func() time.Time) (identity Identity, err error) {
	// Canceled verification is recoverable, even when the token or signature
	// would otherwise be rejected. A permanent failure can discard a grant.
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			identity, err = Identity{}, ctxErr
		}
	}()
	signed, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || len(signed.Signatures) != 1 {
		return Identity{}, ErrInvalidID
	}
	sig := signed.Signatures[0]
	_, critical := sig.Protected.ExtraHeaders["crit"]
	encoded, hasEncoding := sig.Protected.ExtraHeaders["b64"]
	if critical || (hasEncoding && encoded != true) || sig.Protected.KeyID == "" || len(sig.Signature) == 0 {
		return Identity{}, ErrInvalidID
	}
	// Reject malformed or misdirected tokens even while the issuer is offline.
	if _, valid := parseIdentity(signed.UnsafePayloadWithoutVerification(), clientID, nonce, now()); !valid {
		return Identity{}, ErrInvalidID
	}
	keys, err := c.signingKeys(ctx, sig.Protected.KeyID)
	if err != nil {
		return Identity{}, err
	}
	// Several eligible keys can share the same ID during rotation. Only
	// authenticated claims can supply the identity returned to the caller.
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return Identity{}, err
		}
		payload, err := signed.Verify(key)
		if err != nil {
			continue
		}
		identity, valid := parseIdentity(payload, clientID, nonce, now())
		if valid {
			return identity, nil
		}
		break
	}
	return Identity{}, ErrInvalidID
}

func parseIdentity(payload []byte, clientID, nonce string, now time.Time) (Identity, bool) {
	// go-jose/v4/json rejects duplicate names before assigning struct fields.
	var claims identityClaims
	if json.Unmarshal(payload, &claims) != nil {
		return Identity{}, false
	}
	if claims.AuthorizedParty != nil {
		var party string
		if json.Unmarshal(claims.AuthorizedParty, &party) != nil || party != clientID {
			return Identity{}, false
		}
	}
	unix := now.Unix()
	valid := claims.Issuer == Issuer && claims.Subject != "" && (nonce == "" || claims.Nonce == nonce) &&
		clientID != "" && len(claims.Audience) == 1 && claims.Audience[0] == clientID &&
		claims.Exp > unix && claims.Iat > 0 && claims.Iat <= unix+5 && claims.Nbf <= unix+5
	return Identity{Subject: claims.Subject, Email: claims.Email}, valid
}

// Fetch keys with the same deadline, response-size limit and redirect policy as
// the rest of the client. A missing key can become available after rotation.
// Only a failed verification with an eligible key can reject a pending grant.
func (c Client) signingKeys(ctx context.Context, keyID string) ([]*rsa.PublicKey, error) {
	var document json.RawMessage
	if err := c.request(ctx, http.MethodGet, jwksURL, nil, "", &document); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: cannot retrieve ChatGPT identity keys: %v", ErrNetwork, err)
	}
	var keySet struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if json.Unmarshal(document, &keySet) != nil || keySet.Keys == nil {
		return nil, fmt.Errorf("%w: invalid ChatGPT identity key set", ErrNetwork)
	}
	var keys []*rsa.PublicKey
	for _, raw := range keySet.Keys {
		// Reject overlong base64urlUInt encodings, including leading padding
		// bytes that disappear when a JWK is decoded into an RSA public key.
		var wire struct {
			Exponent   string          `json:"e"`
			Operations json.RawMessage `json:"key_ops"`
		}
		if json.Unmarshal(raw, &wire) != nil || len(wire.Exponent) > 6 {
			continue
		}
		if wire.Operations != nil {
			var operations []string
			if json.Unmarshal(wire.Operations, &operations) != nil || !slices.Contains(operations, "verify") ||
				(len(operations) != 1 && !(len(operations) == 2 && slices.Contains(operations, "sign"))) {
				continue
			}
		}
		var key jose.JSONWebKey
		if json.Unmarshal(raw, &key) != nil || key.KeyID != keyID ||
			(key.Use != "" && key.Use != "sig") || (key.Algorithm != "" && key.Algorithm != string(jose.RS256)) {
			continue
		}
		pub, ok := key.Key.(*rsa.PublicKey)
		if ok && pub.N.BitLen() >= 2048 && pub.E >= 3 && pub.E <= math.MaxInt32 && pub.E%2 == 1 {
			keys = append(keys, pub)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: ChatGPT identity signing key unavailable; retry later", ErrNetwork)
	}
	return keys, nil
}
