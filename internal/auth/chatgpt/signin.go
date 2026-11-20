package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/loopback"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

const signInTimeout = 30 * time.Minute

func signInContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, signInTimeout)
}

// SignIn performs an explicit user-initiated authorization. An empty clientID
// registers a new account; a saved ID reauthorizes that registration. showURL
// receives a browser URL containing no saved ID token or other credential.
func (c *Client) SignIn(ctx context.Context, clientID string, showURL func(string) error) error {
	return c.signIn(ctx, clientID, false, showURL)
}

// EnablePlan explicitly requests renewed consent for an existing registration.
func (c *Client) EnablePlan(ctx context.Context, clientID string, showURL func(string) error) error {
	if clientID == "" {
		return errors.New("enabling plan usage requires an existing --account CLIENT_ID")
	}
	return c.signIn(ctx, clientID, true, showURL)
}

func (c *Client) signIn(ctx context.Context, clientID string, consent bool, showURL func(string) error) error {
	ctx, cancel := signInContext(ctx)
	defer cancel()

	var hostID, generation, selection string
	err := c.withStore(ctx, func(accounts *accountStore) error {
		if clientID != "" {
			previous, err := accounts.credential(clientID)
			if err != nil {
				return err
			}
			if previous.Subject == "" && !previous.PendingRegistration {
				return errors.New("ChatGPT registration has no validated identity")
			}
			generation = previous.Generation
		}
		if err := accounts.ensureHost(); err != nil {
			return err
		}
		hostID, selection = accounts.HostID, accounts.Selection
		return nil
	})
	if err != nil {
		return err
	}

	auth := oauth.NewAuthorization(clientID, hostID, consent)
	listener, err := loopback.Listen(auth.State, clientID)
	if err != nil {
		return err
	}
	defer listener.Close()

	redirect := listener.RedirectURI()
	if err := showURL(auth.URL(redirect)); err != nil {
		return err
	}
	callback, err := listener.Wait(ctx)
	if err != nil {
		return err
	}

	// Browser interaction leaves the store unlocked. From code exchange
	// through publication, sign-in serializes with refresh and sign-out.
	return c.withStore(ctx, func(accounts *accountStore) error {
		if accounts.HostID != hostID {
			return errors.New("ChatGPT host identity changed during sign-in")
		}
		previous, exists := accounts.Accounts[callback.ClientID]
		if clientID == "" {
			if exists {
				return fmt.Errorf("ChatGPT registration %q already exists. Reauthorize it with roast chatgpt signin --account CLIENT_ID", callback.ClientID)
			}
			// Save the issued ID before consuming the one-use code, so a
			// failed exchange can be retried for the same registration.
			previous = credential{Account: Account{ClientID: callback.ClientID, PendingRegistration: true}}
			if err := accounts.saveCredential(&previous); err != nil {
				return fmt.Errorf("could not save pending ChatGPT registration: %w", err)
			}
		} else if !exists || previous.Generation != generation {
			return errors.New("ChatGPT credentials changed during sign-in; retry sign-in explicitly")
		}
		return c.finishSignIn(ctx, accounts, previous, auth, redirect, callback.Code, selection)
	})
}

func (c *Client) finishSignIn(ctx context.Context, accounts *accountStore, previous credential, auth oauth.Authorization, redirect, code, selection string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The one-use code can already be consumed when the user cancels. Read
	// the bounded exchange response so an unpublished grant can be revoked.
	grantCtx, stop := tokenGrantContext(ctx)
	tokens, err := c.provider.Exchange(grantCtx, previous.ClientID, code, auth.Verifier, redirect)
	stop()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(ctxErr, err)
	}
	receivedAt := c.now()
	var next credential
	if err == nil {
		next, err = c.validateSignInGrant(ctx, previous, auth.Nonce, tokens, receivedAt)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cause := fmt.Errorf("%w for ChatGPT account %q. Retry with roast chatgpt signin --account CLIENT_ID", err, previous.ClientID)
		return c.revokeUnpublishedGrant(ctx, previous, tokens, cause)
	}

	if accounts.Selection == selection {
		accounts.setActive(next.ClientID)
	}
	if err := accounts.saveCredentialContext(ctx, &next); err != nil {
		cause := fmt.Errorf("could not confirm saving ChatGPT credentials for account %q: %w", next.ClientID, err)
		if errors.Is(err, storage.ErrPublished) {
			// The new credentials are visible. Revoking would sign the user out.
			return cause
		}
		return c.revokeUnpublishedGrant(ctx, previous, tokens, cause)
	}
	if !next.planEnabled() {
		return fmt.Errorf("ChatGPT plan usage is disabled for signed-in account %q. Run roast chatgpt signin --account CLIENT_ID --enable-plan or configure another billing path", next.ClientID)
	}
	return nil
}

func (c *Client) validateSignInGrant(ctx context.Context, previous credential, nonce string, tokens oauth.TokenResponse, receivedAt time.Time) (credential, error) {
	identity, err := c.provider.ValidateID(ctx, tokens.IDToken, previous.ClientID, nonce, c.now)
	if err != nil {
		return credential{}, err
	}
	if previous.Subject != "" && identity.Subject != previous.Subject {
		return credential{}, errors.New("ChatGPT account identity changed during reauthorization")
	}

	next := credential{Account: Account{ClientID: previous.ClientID, Subject: identity.Subject, Email: identity.Email}}
	if err := next.applyTokenResponse(tokens, receivedAt); err != nil {
		return credential{}, err
	}
	return next, nil
}

func (c *Client) revokeUnpublishedGrant(ctx context.Context, previous credential, tokens oauth.TokenResponse, cause error) error {
	token, tokenType := (credential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken}).revocationToken()
	if token == "" {
		return cause
	}
	if previous.Subject != "" || previous.hasRevocableGrant() {
		// A verified registration may still have a grant at OpenAI, including
		// after an unconfirmed sign-out. Revocation could end that session.
		return errors.Join(cause, unconfirmedRevocation(previous.ClientID))
	}
	return c.revokeDiscardedToken(ctx, previous.ClientID, token, tokenType, cause)
}

func (c *Client) revokeDiscardedToken(ctx context.Context, clientID, token, tokenType string, cause error) error {
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.provider.Revoke(revokeCtx, clientID, token, tokenType); err != nil {
		return errors.Join(cause, unconfirmedRevocation(clientID))
	}
	return cause
}

func unconfirmedRevocation(clientID string) error {
	return fmt.Errorf("remote revocation was not confirmed for ChatGPT account %q. Disconnect Roast in ChatGPT Settings", clientID)
}
