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
	if c.selected == "" {
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
		if rotated {
			cause := fmt.Errorf("%w: ChatGPT returned a malformed refresh response with a replacement token: %v", ErrAuth, err)
			cause = c.revokeDiscardedToken(ctx, saved.ClientID, tokens.RefreshToken, "refresh_token", cause)
			return "", accounts.clearStoredGrant(&saved, cause)
		}
		if oauth.InvalidRefreshGrant(err) {
			return "", accounts.clearStoredGrant(&saved, err)
		}
		return "", err
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
		case errors.Is(err, errInvalidRefresh):
			return "", fmt.Errorf("%w: malformed ChatGPT refresh response. Existing grant retained: %w", ErrNetwork, err)
		case errors.Is(err, errRefreshIdentityChanged):
			return "", accounts.clearStoredGrant(&saved, err)
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

// A directory sync can report failure after publishing the checkpoint. Keep
// a published replacement so the next call can resume its validation.
func (c *Client) refreshCheckpointError(ctx context.Context, accounts *accountStore, saved *credential, previousGeneration string, writeErr error) error {
	cause := fmt.Errorf("ChatGPT refresh completed but its replacement could not be durably saved: %w", writeErr)
	if errors.Is(writeErr, storage.ErrPublished) {
		return fmt.Errorf("%w. Repair credential storage and retry", cause)
	}

	// The atomic rename did not happen. The replacement cannot be recovered
	// from disk, even when a storage failure also prevents reading the old file.
	cause = c.revokeDiscardedToken(ctx, saved.ClientID, saved.Pending.Response.RefreshToken, "refresh_token", cause)
	current, err := c.loadStore()
	if err == nil && current.HostID == accounts.HostID && current.Accounts[saved.ClientID].Generation == previousGeneration {
		return accounts.clearStoredGrant(saved, cause)
	}
	return fmt.Errorf("%w. Roast could not confirm local credential cleanup for account %q. Repair credential storage, then run roast chatgpt signin --account CLIENT_ID before retrying", cause, saved.ClientID)
}

func (c *Client) acceptPending(ctx context.Context, accounts *accountStore, saved *credential) error {
	pending := saved.Pending
	receivedAt := pending.ReceivedAt
	if receivedAt.IsZero() || receivedAt.Unix() <= 0 {
		return c.discardInvalidRefresh(ctx, accounts, saved, errors.New("invalid pending ChatGPT token receipt time"))
	}

	if receivedAt.After(c.now().Add(5 * time.Second)) {
		return fmt.Errorf("%w: pending ChatGPT token was received ahead of the local clock; check the clock and retry", ErrNetwork)
	}

	next, err := c.validatedRefresh(ctx, *saved, pending.Response, receivedAt)
	switch {
	case errors.Is(err, errInvalidRefresh), errors.Is(err, errRefreshIdentityChanged):
		return c.discardInvalidRefresh(ctx, accounts, saved, err)
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

// Discard a malformed response while retaining the validated grant when the
// provider did not issue a replacement refresh token.
func (c *Client) discardInvalidRefresh(ctx context.Context, accounts *accountStore, saved *credential, cause error) error {
	replacement := saved.Pending.Response.RefreshToken
	if replacement != "" && replacement != saved.RefreshToken && oauth.ValidRefreshToken(replacement) {
		cause = c.revokeDiscardedToken(ctx, saved.ClientID, replacement, "refresh_token", cause)
		return accounts.clearStoredGrant(saved, cause)
	}
	if errors.Is(cause, errRefreshIdentityChanged) {
		return accounts.clearStoredGrant(saved, cause)
	}
	saved.Pending = nil
	// Removing an invalid checkpoint keeps the underlying grant unchanged.
	accounts.Accounts[saved.ClientID] = *saved
	if err := accounts.save(); err != nil {
		return errors.Join(cause, fmt.Errorf("could not discard the malformed ChatGPT refresh response. Repair credential storage and retry: %w", err))
	}
	return fmt.Errorf("%w: malformed ChatGPT refresh response. Existing grant retained: %w", ErrNetwork, cause)
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
