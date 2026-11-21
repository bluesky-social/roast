package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// Token returns a usable plan access token for the registration pinned by New.
func (c *Client) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.selected == "" {
		accounts, err := c.loadStore()
		if err != nil {
			return "", err
		}
		for _, saved := range accounts.Accounts {
			if saved.Pending != nil && saved.Pending.RevokeOnly {
				return "", fmt.Errorf("%w: no account selected. Registration %q has a pending disconnection. Run roast chatgpt signout -- CLIENT_ID with that registration ID", ErrAuth, saved.ClientID)
			}
		}
		if len(accounts.Accounts) != 0 {
			return "", fmt.Errorf("%w: no account selected. Run roast chatgpt status, then select an account or resume sign-in with roast chatgpt signin --account CLIENT_ID", ErrAuth)
		}
		return "", fmt.Errorf("%w: no account selected; run roast chatgpt signin", ErrAuth)
	}
	var token string
	err := c.withStore(ctx, func(accounts *accountStore) error {
		var err error
		token, err = c.selectedToken(ctx, accounts)
		return err
	})
	if err != nil && !errors.Is(err, ErrNetwork) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", ErrAuth, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if !errors.Is(err, ctxErr) {
			err = errors.Join(ctxErr, err)
		}
		return "", err
	}
	return token, err
}

// selectedToken runs under the session lock to serialize refresh rotation.
func (c *Client) selectedToken(ctx context.Context, accounts *accountStore) (string, error) {
	saved, err := accounts.credential(c.selected)
	if err != nil {
		return "", err
	}
	if saved.Pending != nil {
		if err := c.acceptPending(ctx, accounts, &saved); err != nil {
			return "", err
		}
	}
	if !saved.signedIn() {
		return "", errors.New("ChatGPT account is signed out; run roast chatgpt signin --account CLIENT_ID")
	}
	if !saved.planEnabled() {
		return "", fmt.Errorf("ChatGPT plan usage scope is missing for account %q. Run roast chatgpt signin --account CLIENT_ID --enable-plan", saved.ClientID)
	}
	if !oauth.ValidAccessToken(saved.AccessToken) || !oauth.ValidRefreshToken(saved.RefreshToken) {
		return "", fmt.Errorf("invalid saved ChatGPT token format for account %q. Run roast chatgpt signin --account CLIENT_ID", saved.ClientID)
	}

	now := c.now()
	// Renew in the last minute of the token's life, once the issuer permits rotation.
	if now.Before(saved.ExpiresAt.Add(-time.Minute)) || now.Unix() < saved.EarliestRefreshAt {
		if !now.Before(saved.ExpiresAt) {
			return "", errors.New("ChatGPT token expired before refresh is permitted; retry after earliest_refresh_at")
		}
		return saved.AccessToken, nil
	}
	return c.refresh(ctx, accounts, saved)
}

var errInvalidRefresh = errors.New("invalid ChatGPT refresh response")
var errRefreshIdentityChanged = errors.New("refreshed ChatGPT identity changed")

func (c *Client) refresh(ctx context.Context, accounts *accountStore, saved credential) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Once the issuer consumes the old token, finish reading its replacement
	// within the HTTP client's deadline, even if the review is canceled.
	grantCtx, stop := tokenGrantContext(ctx)
	tokens, err := c.provider.Refresh(grantCtx, saved.ClientID, saved.RefreshToken)
	stop()
	rotated := tokens.RefreshToken != "" && tokens.RefreshToken != saved.RefreshToken
	if err != nil {
		if errors.Is(err, oauth.ErrAmbiguousRotation) {
			// A replacement was reported but cannot be used. Revoke a known
			// access token and direct the user to Settings for the renewable grant.
			access := tokens.AccessToken
			if access == "" {
				access = saved.AccessToken
			}
			return "", c.discardGrant(ctx, accounts, &saved, access, "access_token", oauth.ErrAmbiguousRotation, false)
		}
		if rotated {
			cause := fmt.Errorf("%w: ChatGPT returned a malformed refresh response with a replacement token: %v", ErrAuth, err)
			if !oauth.ValidRefreshToken(tokens.RefreshToken) {
				return "", accounts.clearStoredGrant(&saved, errors.Join(cause, unconfirmedRevocation(saved.ClientID)))
			}
			return "", c.discardGrant(ctx, accounts, &saved, tokens.RefreshToken, "refresh_token", cause, false)
		}
		if oauth.InvalidRefreshGrant(err) {
			if c.now().Before(saved.ExpiresAt) {
				return "", c.discardGrant(ctx, accounts, &saved, saved.AccessToken, "access_token", err, false)
			}
			cause := fmt.Errorf("%w. If an earlier renewal was interrupted, disconnect Roast in ChatGPT Settings to revoke any unseen replacement", err)
			return "", accounts.clearStoredGrant(&saved, cause)
		}
		if oauth.DefinitiveGrantRejection(err) {
			return "", err
		}
		// The issuer may have rotated the grant before the reply arrived. An
		// access token alone does not establish rotation: revoking it can also
		// revoke the existing refresh token (RFC 7009 section 2.1).
		return "", fmt.Errorf("%w. If renewal keeps failing, disconnect Roast in ChatGPT Settings and sign in again", err)
	}
	receivedAt := c.now()
	if rotated {
		// Persist the replacement before fetching identity keys so a later
		// call can resume validation without replaying a consumed token.
		previousGeneration := saved.Generation
		saved.Pending = &pendingRefresh{Response: tokens, ReceivedAt: receivedAt}
		if err := accounts.saveCredential(&saved); err != nil {
			return "", c.refreshCheckpointError(ctx, accounts, &saved, previousGeneration, err)
		}
		if err := c.acceptPending(ctx, accounts, &saved); err != nil {
			return "", err
		}
	} else {
		// Without rotation, validate the entire response before writing.
		// A rejected replacement must leave the sign-in snapshot intact.
		next, err := c.validatedRefresh(ctx, saved, tokens, receivedAt)
		switch {
		case errors.Is(err, errInvalidRefresh), errors.Is(err, errRefreshIdentityChanged):
			return "", c.discardInvalidRefresh(ctx, accounts, &saved, tokens, err)
		case err != nil:
			return "", err
		}
		if err := accounts.saveCredential(&next); err != nil {
			return "", fmt.Errorf("could not persist validated ChatGPT credentials; repair credential storage and retry: %w", err)
		}
		saved = next
	}

	if !saved.planEnabled() {
		return "", errors.New("ChatGPT plan usage permission was removed; enable it explicitly before inference")
	}
	// Keep the rotated credentials if validation outlasted the access token.
	if !c.now().Before(saved.ExpiresAt) {
		return "", fmt.Errorf("%w: refreshed token expired during validation; retry", ErrNetwork)
	}
	return saved.AccessToken, nil
}

// The keyring can report a failure after saving the checkpoint. Keep a
// replacement that may be visible so the next call can resume its validation.
func (c *Client) refreshCheckpointError(ctx context.Context, accounts *accountStore, saved *credential, previousGeneration string, writeErr error) error {
	cause := fmt.Errorf("ChatGPT refresh completed for account %q but saving its replacement could not be confirmed: %w", saved.ClientID, writeErr)
	if errors.Is(writeErr, storage.ErrPublished) {
		current, err := loadAccountStore(accounts.storage)
		if err != nil || current.HostID != accounts.HostID || current.Accounts[saved.ClientID].Generation != previousGeneration {
			return fmt.Errorf("%w. Repair credential storage and retry. If renewal still fails, disconnect Roast in ChatGPT Settings and sign in again", cause)
		}
		retryErr := accounts.saveCredential(saved)
		if retryErr == nil {
			return fmt.Errorf("%w. The replacement is now saved. Retry to finish validation", cause)
		}
		cause = errors.Join(cause, retryErr)
		if errors.Is(retryErr, storage.ErrPublished) {
			return fmt.Errorf("%w. Repair credential storage and retry. If renewal still fails, disconnect Roast in ChatGPT Settings and sign in again", cause)
		}
	}

	// Publication failed. A later read can only recover the earlier grant.
	cause = c.revokeDiscardedToken(ctx, saved.ClientID, saved.Pending.Response.RefreshToken, "refresh_token", cause)
	current, err := loadAccountStore(accounts.storage)
	if err == nil && current.HostID == accounts.HostID && current.Accounts[saved.ClientID].Generation == previousGeneration {
		return accounts.clearStoredGrant(saved, cause)
	}
	return fmt.Errorf("%w. Roast could not confirm local credential cleanup for account %q. Repair credential storage, then run roast chatgpt signin --account CLIENT_ID before retrying", cause, saved.ClientID)
}

func (c *Client) acceptPending(ctx context.Context, accounts *accountStore, saved *credential) error {
	pending := saved.Pending
	if pending.RevokeOnly {
		// The checkpoint holds only the token that may be sent to revocation.
		grant := credential{RefreshToken: pending.Response.RefreshToken, AccessToken: pending.Response.AccessToken}
		token, kind := grant.revocationToken()
		cause := errors.New("finishing an interrupted ChatGPT token revocation")
		if token == "" {
			return accounts.clearStoredGrant(saved, errors.Join(cause, unconfirmedRevocation(saved.ClientID)))
		}
		return c.discardGrant(ctx, accounts, saved, token, kind, cause, true)
	}
	receivedAt := pending.ReceivedAt
	if receivedAt.IsZero() || receivedAt.Unix() <= 0 {
		return c.discardInvalidRefresh(ctx, accounts, saved, pending.Response, errors.New("invalid pending ChatGPT token receipt time"))
	}

	if receivedAt.After(c.now().Add(5 * time.Second)) {
		return fmt.Errorf("%w: pending ChatGPT token was received ahead of the local clock; check the clock and retry", ErrNetwork)
	}

	next, err := c.validatedRefresh(ctx, *saved, pending.Response, receivedAt)
	switch {
	case errors.Is(err, errInvalidRefresh), errors.Is(err, errRefreshIdentityChanged):
		return c.discardInvalidRefresh(ctx, accounts, saved, pending.Response, err)
	case err != nil:
		return err
	}
	if err := accounts.saveCredential(&next); err != nil {
		return fmt.Errorf("could not persist validated ChatGPT credentials; repair credential storage and retry: %w", err)
	}
	*saved = next
	return nil
}

func (c *Client) validatedRefresh(ctx context.Context, previous credential, response oauth.TokenResponse, receivedAt time.Time) (credential, error) {
	next := previous
	if err := next.applyRefreshResponse(response, receivedAt); err != nil {
		return credential{}, fmt.Errorf("%w: %w", errInvalidRefresh, err)
	}
	if response.IDToken != "" {
		// Validate identity at receipt so a JWKS outage can outlast the ID
		// token. Access-token expiry is still checked against the live clock.
		identity, err := c.provider.ValidateID(ctx, response.IDToken, previous.ClientID, "", func() time.Time { return receivedAt })
		if errors.Is(err, oauth.ErrInvalidID) {
			return credential{}, fmt.Errorf("%w: %w", errInvalidRefresh, err)
		}
		if err != nil {
			return credential{}, err
		}
		if identity.Subject != previous.Subject {
			// A verified different subject calls the account binding of the
			// existing grant into question, even without token rotation.
			return credential{}, errRefreshIdentityChanged
		}
	}
	next.Pending = nil
	return next, nil
}

// Reject a response that cannot be used for this account. A malformed reply
// without rotation leaves the previously validated grant intact.
func (c *Client) discardInvalidRefresh(ctx context.Context, accounts *accountStore, saved *credential, response oauth.TokenResponse, cause error) error {
	checkpointed := saved.Pending != nil
	if replacement := response.RefreshToken; replacement != "" && replacement != saved.RefreshToken && oauth.ValidRefreshToken(replacement) {
		return c.discardGrant(ctx, accounts, saved, replacement, "refresh_token", cause, checkpointed)
	}
	if errors.Is(cause, errRefreshIdentityChanged) {
		if response.AccessToken == saved.AccessToken {
			// Neither token is new. Revoking either could disconnect the previously verified account.
			return accounts.clearStoredGrant(saved, errors.Join(cause, unconfirmedRevocation(saved.ClientID)))
		}
		return c.discardGrant(ctx, accounts, saved, response.AccessToken, "access_token", cause, checkpointed)
	}
	if checkpointed {
		saved.Pending = nil
		// Removing an invalid checkpoint keeps the underlying grant unchanged.
		accounts.Accounts[saved.ClientID] = *saved
		if err := accounts.save(context.Background()); err != nil {
			return errors.Join(cause, fmt.Errorf("could not discard the malformed ChatGPT refresh response. Repair credential storage and retry: %w", err))
		}
	}
	return fmt.Errorf("%w: malformed ChatGPT refresh response. Existing grant retained: %w", ErrNetwork, cause)
}

// A rejected token can only be sent to revocation. Saving it first lets the
// next command finish cleanup if Roast exits during the request.
func (c *Client) discardGrant(ctx context.Context, accounts *accountStore, saved *credential, token, kind string, cause error, checkpointed bool) error {
	if err := accounts.markForRevocation(saved, token, kind, kind == "access_token"); err != nil &&
		!(errors.Is(err, storage.ErrPublished) && accounts.confirmRevocationCheckpoint(*saved)) {
		cause = errors.Join(cause, fmt.Errorf("could not record the rejected ChatGPT replacement; repair credential storage: %w", err))
		if checkpointed {
			// The durable copy may still allow validation. Retry the marker
			// before revoking a token that recovery could otherwise publish.
			return errors.Join(cause, unconfirmedRevocation(saved.ClientID))
		}
		// A malformed reply has never been saved for validation. We can revoke
		// it from memory and attempt local cleanup even when storage failed.
	}
	cause = c.revokeDiscardedToken(ctx, saved.ClientID, token, kind, cause)
	// Erase the rejected secret after a completed attempt. If the provider
	// could not confirm revocation, the error directs the user to Settings.
	return accounts.clearStoredGrant(saved, cause)
}

// Persist the only permitted use of a rejected token before contacting the
// issuer. A marker may be visible even when a keyring write reports an error.
func (accounts *accountStore) markForRevocation(saved *credential, token, kind string, unknownRefresh bool) error {
	saved.clearGrant()
	saved.Pending = &pendingRefresh{RevokeOnly: true, UnknownRefresh: unknownRefresh}
	if kind == "refresh_token" {
		saved.Pending.Response.RefreshToken = token
	} else {
		saved.Pending.Response.AccessToken = token
	}
	return accounts.saveCredential(saved)
}

// clearStoredGrant runs under the session lock. It clears any pending tokens
// and the previously saved grant, preserving the registration for sign-in.
func (accounts *accountStore) clearStoredGrant(saved *credential, cause error) error {
	saved.clearGrant()
	if err := accounts.saveCredential(saved); err != nil {
		return errors.Join(cause, fmt.Errorf("could not confirm local credential cleanup for account %q: %w. Repair credential storage, then run roast chatgpt signin --account CLIENT_ID before retrying", saved.ClientID, err))
	}
	return fmt.Errorf("%w. Local credentials were cleared for account %q. Reauthorize it with roast chatgpt signin --account CLIENT_ID", cause, saved.ClientID)
}
