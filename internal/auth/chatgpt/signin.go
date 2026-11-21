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

func (c *Client) signIn(ctx context.Context, clientID string, consent bool, showURL func(string) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()
	// Detached cleanup can finish after the user cancels. Report both
	// failures, including when recovering a pending grant before opening a browser.
	defer func() {
		if ctxErr := ctx.Err(); err != nil && ctxErr != nil && !errors.Is(err, ctxErr) {
			err = errors.Join(err, ctxErr)
		}
	}()

	var hostID, generation, selection string
	err = c.withStore(ctx, func(accounts *accountStore) error {
		if clientID != "" {
			previous, err := accounts.credential(clientID)
			if err != nil {
				return err
			}
			if previous.Pending != nil {
				// Recover an interrupted rotation before reauthorization can replace it.
				if err := c.acceptPending(ctx, accounts, &previous); err != nil {
					if previous.Pending == nil {
						return err // Cleanup already carries the applicable recovery instructions.
					}
					return fmt.Errorf("resolving a pending renewal for ChatGPT account %q: %w. If recovery keeps failing, run roast chatgpt signout CLIENT_ID, then roast chatgpt signin --account CLIENT_ID", previous.ClientID, err)
				}
			}
			if previous.Subject == "" && !previous.PendingRegistration {
				return errors.New("ChatGPT registration has no validated identity")
			}
			generation = previous.Generation
		}
		if err := accounts.ensureHost(ctx); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	displayErr := showURL(auth.URL(redirect))
	waitCtx := ctx
	if displayErr != nil {
		if clientID != "" {
			return displayErr
		}
		// The URL may already have reached the browser. Drain an accepted
		// registration without waiting for a new callback or exchanging its code.
		var stop context.CancelFunc
		waitCtx, stop = context.WithCancel(ctx)
		stop()
	}
	callback, err := listener.Wait(waitCtx)
	if err != nil {
		if displayErr != nil {
			err = displayErr
		}
		if clientID == "" {
			return registrationRecovery(err)
		}
		return err
	}

	// Browser interaction leaves the store unlocked. From code exchange
	// through publication, sign-in serializes with refresh and sign-out.
	storeCtx := ctx
	checkpointed := false
	if clientID == "" {
		// The issuer has already created a registration. Allow enough time to finish a concurrent
		// token exchange and identity-key request, even if sign-in is canceled.
		var cancelStore context.CancelFunc
		storeCtx, cancelStore = context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancelStore()
	}
	entered := false
	finish := func(accounts *accountStore) error {
		entered = true
		if accounts.HostID != hostID {
			return errors.New("ChatGPT host identity changed during sign-in")
		}
		previous, exists := accounts.Accounts[callback.ClientID]
		if clientID == "" {
			if exists {
				checkpointed = true
				return fmt.Errorf("ChatGPT registration %q already exists. Reauthorize it with roast chatgpt signin --account CLIENT_ID", callback.ClientID)
			}
			// Save the issued ID before consuming the one-use code, so a
			// failed exchange can be retried for the same registration. Complete
			// this local checkpoint if canceled: the issuer already created the ID.
			previous = credential{Account: Account{ClientID: callback.ClientID, PendingRegistration: true}}
			if err := accounts.saveCredentialContext(storeCtx, &previous); err != nil {
				if errors.Is(err, storage.ErrPublished) {
					checkpointed = true
					return registrationRecovery(fmt.Errorf("saving registration %q in the keyring could not be confirmed. Repair credential storage before retrying: %w", previous.ClientID, err))
				}
				return err
			}
			checkpointed = true
			if displayErr != nil {
				return fmt.Errorf("ChatGPT registration %q is saved. Resume with roast chatgpt signin --account CLIENT_ID: %w", previous.ClientID, displayErr)
			}
		} else if !exists || previous.Generation != generation {
			return errors.New("ChatGPT credentials changed during sign-in; retry sign-in explicitly")
		}
		return c.finishSignIn(ctx, accounts, previous, auth, redirect, callback.Code, selection)
	}
	for {
		err = c.withStore(storeCtx, finish)
		// Retry only acquisition for a newly issued ID. Once the callback
		// starts it can write credentials and consume the one-use code.
		if clientID != "" || entered || storeCtx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			break
		}
	}
	if err != nil && clientID == "" {
		if displayErr != nil && !errors.Is(err, displayErr) {
			err = errors.Join(err, displayErr)
		}
		if !checkpointed {
			return registrationRecovery(fmt.Errorf("could not record issued ChatGPT registration %q: %w", callback.ClientID, err))
		}
	}
	return err
}

func registrationRecovery(err error) error {
	return fmt.Errorf("%w. If you already approved Roast in the browser, check roast chatgpt status. If the registration is missing, disconnect it in ChatGPT Settings and run roast chatgpt signin again", err)
}

func (c *Client) finishSignIn(ctx context.Context, accounts *accountStore, previous credential, auth oauth.Authorization, redirect, code, selection string) error {
	if err := ctx.Err(); err != nil {
		if previous.PendingRegistration {
			return fmt.Errorf("ChatGPT registration %q is saved. Resume with roast chatgpt signin --account CLIENT_ID: %w", previous.ClientID, err)
		}
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
	token, kind := (credential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken}).revocationToken()
	if err == nil && previous.PendingRegistration && token != "" {
		// Key retrieval can outlive the process. Keep the first grant exclusively
		// for revocation until its identity has been verified.
		pending := previous
		if saveErr := accounts.markForRevocation(&pending, token, kind, kind == "access_token"); saveErr != nil &&
			!(errors.Is(saveErr, storage.ErrPublished) && accounts.confirmRevocationCheckpoint(pending)) {
			err = fmt.Errorf("could not save the first ChatGPT grant for recovery: %w", saveErr)
		}
	}
	var next credential
	if err == nil {
		next, err = c.validateSignInGrant(ctx, previous, auth.Nonce, tokens, receivedAt)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cause := fmt.Errorf("%w for ChatGPT account %q. Retry with roast chatgpt signin --account CLIENT_ID", err, previous.ClientID)
		return c.revokeUnpublishedGrant(ctx, accounts, previous, token, kind, cause)
	}

	// Cleanup after a failed write must preserve the selection from before sign-in.
	publication := *accounts
	if publication.Selection == selection {
		publication.setActive(next.ClientID)
	}
	if err := publication.saveCredentialContext(ctx, &next); err != nil {
		cause := fmt.Errorf("could not confirm saving ChatGPT credentials for account %q: %w", next.ClientID, err)
		if errors.Is(err, storage.ErrPublished) {
			// The grant may already be saved. Revoking it could end that session.
			if !previous.PendingRegistration {
				return fmt.Errorf("%w. Check roast chatgpt status and repair credential storage. If the account still cannot be used, disconnect its registration in ChatGPT Settings and sign in again", cause)
			}
			return registrationRecovery(cause)
		}
		return c.revokeUnpublishedGrant(ctx, accounts, previous, token, kind, cause)
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

	// Plan use requires the permission in the token response. Requesting it
	// in the browser does not establish that the user granted it.
	next := credential{Account: Account{ClientID: previous.ClientID, Subject: identity.Subject, Email: identity.Email}}
	if err := next.applyTokenResponse(tokens, receivedAt); err != nil {
		return credential{}, err
	}
	return next, nil
}

func (c *Client) revokeUnpublishedGrant(ctx context.Context, accounts *accountStore, previous credential, token, tokenType string, cause error) error {
	if token == "" {
		// After an interrupted exchange the issuer may have consumed the code.
		// A definitive OAuth rejection did not issue a grant.
		if !oauth.DefinitiveGrantRejection(cause) {
			return errors.Join(cause, unconfirmedRevocation(previous.ClientID))
		}
		return cause
	}
	if previous.Subject != "" || previous.hasRevocableGrant() {
		// A verified registration may still have a grant at OpenAI, including
		// after an unconfirmed sign-out. Revocation could end that session.
		return errors.Join(cause, unconfirmedRevocation(previous.ClientID))
	}
	return c.discardGrant(ctx, accounts, &previous, token, tokenType, cause, false)
}

func (c *Client) revokeDiscardedToken(ctx context.Context, clientID, token, tokenType string, cause error) error {
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// An access token alone cannot establish what happened to a renewable grant.
	if err := c.provider.Revoke(revokeCtx, clientID, token, tokenType); err != nil || tokenType == "access_token" {
		return errors.Join(cause, unconfirmedRevocation(clientID))
	}
	return fmt.Errorf("%w. Check connected apps in ChatGPT Settings", cause)
}

func unconfirmedRevocation(clientID string) error {
	return fmt.Errorf("remote revocation was not confirmed for ChatGPT account %q. Disconnect Roast in ChatGPT Settings", clientID)
}
