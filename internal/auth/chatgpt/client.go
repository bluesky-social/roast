// Package chatgpt owns Roast's independent Sign in with ChatGPT registrations.
package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/loopback"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

var (
	ErrAuth    = oauth.ErrAuth
	ErrNetwork = oauth.ErrNetwork
)

type Client struct {
	store    storage.Store
	selected string
	provider oauth.Client
	now      func() time.Time
}

// The issuer may already have consumed a one-use code or refresh token when
// the caller cancels. Allow a short grace period to receive its reply while
// keeping an unresponsive sign-in promptly cancelable.
func tokenGrantContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stop := context.AfterFunc(parent, func() {
		select {
		case <-time.After(time.Second):
			cancel()
		case <-ctx.Done():
		}
	})
	return ctx, func() { stop(); cancel() }
}

// New opens Roast's registration store and pins the active registration.
func New() (*Client, error) {
	store, err := storage.Open()
	if err != nil {
		return nil, err
	}
	c := &Client{
		store:    store,
		provider: oauth.NewClient(),
		now:      time.Now,
	}
	accounts, err := c.loadStore()
	if err != nil {
		return nil, err
	}
	c.selected = accounts.Active
	return c, nil
}

type Model = oauth.Model

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	return c.provider.Models(ctx, token)
}

// Account is the registration information that can be shown by the CLI.
type Account struct {
	PendingRegistration bool      `json:"pending_registration,omitempty"`
	ClientID            string    `json:"client_id"`
	Subject             string    `json:"subject"`
	Email               string    `json:"email,omitempty"`
	Active              bool      `json:"-"`
	SignedIn            bool      `json:"-"`
	PlanEnabled         bool      `json:"-"`
	ExpiresAt           time.Time `json:"expires_at"`
}

// ValidAccountID reports whether an ID has the form of an issued registration.
func ValidAccountID(id string) bool {
	return loopback.ValidIssuedClientID(id)
}

// Status reads the saved registrations and their current local grant state.
func (c *Client) Status(ctx context.Context) ([]Account, error) {
	accounts, err := c.loadStore()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(accounts.Accounts))
	for _, id := range slices.Sorted(maps.Keys(accounts.Accounts)) {
		saved := accounts.Accounts[id]
		a := saved.Account
		a.Active = id == accounts.Active
		a.SignedIn = saved.signedIn()
		a.PlanEnabled = saved.planEnabled()
		out = append(out, a)
	}
	return out, nil
}

// Select chooses the registration that future clients will use.
func (c *Client) Select(ctx context.Context, id string) error {
	return c.withStore(ctx, func(accounts *accountStore) error {
		saved, err := accounts.credential(id)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !saved.signedIn() {
			return errors.New("account is signed out; run roast chatgpt signin --account CLIENT_ID")
		}
		accounts.setActive(id)
		return accounts.save(ctx)
	})
}

// SignOut attempts revocation and clears the local grant. An empty ID uses
// the registration this client selected before waiting for the store lock.
// Success confirms local cleanup and an accepted revocation request for any
// known token. Only the issuer can confirm the state of an unseen replacement.
func (c *Client) SignOut(ctx context.Context, id string) error {
	if id == "" {
		id = c.selected
	}
	if id == "" {
		return errors.New("no active ChatGPT account. Use roast chatgpt status to find the registration, then run roast chatgpt signout CLIENT_ID")
	}
	return c.withStore(ctx, func(accounts *accountStore) error {
		saved, err := accounts.credential(id)
		if err != nil {
			return err
		}

		token, tokenType := saved.revocationToken()
		unknownRefresh := saved.Pending != nil && saved.Pending.UnknownRefresh
		var remoteErr error
		if token == "" && (saved.PendingRegistration || (saved.Pending != nil && saved.Pending.RevokeOnly)) {
			// A prior exchange or revocation may already have reached the issuer.
			remoteErr = unconfirmedRevocation(id)
		}
		if token != "" {
			// A crash during revocation must never leave a locally usable grant.
			if err := accounts.markForRevocation(&saved, token, tokenType, unknownRefresh); err != nil {
				if !errors.Is(err, storage.ErrPublished) || !accounts.confirmRevocationCheckpoint(saved) {
					return errors.Join(fmt.Errorf("could not confirm saving the pending sign-out: %w. Repair credential storage and retry with roast chatgpt signout -- %s", err, id), unconfirmedRevocation(id))
				}
			}
			// Revocation precedes local cleanup. Bound it across discovery and
			// retries so another command can acquire the session lock.
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := c.provider.Revoke(revokeCtx, id, token, tokenType)
			cancel()
			if err != nil || unknownRefresh {
				remoteErr = errors.Join(unconfirmedRevocation(id), ctx.Err())
			}
		}

		// Keep the registration selected until the final save so a sign-out
		// interrupted at its revocation checkpoint can be retried without an ID.
		if accounts.Active == id {
			accounts.setActive("")
		}
		saved.clearGrant()
		if err := accounts.saveCredential(&saved); err != nil {
			return errors.Join(fmt.Errorf("could not confirm local sign-out from ChatGPT account %q: %w. Repair credential storage and retry with roast chatgpt signout -- %s", id, err, id), remoteErr)
		}
		if remoteErr != nil {
			return fmt.Errorf("ChatGPT account %q signed out locally. %w", id, remoteErr)
		}
		return nil
	})
}

// Verify the checkpoint before revoking when the keyring could not confirm its
// write or finish cleanup. The caller still holds the account lock.
func (s *accountStore) confirmRevocationCheckpoint(saved credential) bool {
	current, err := loadAccountStore(s.storage)
	if err != nil {
		return false
	}
	checkpoint := current.Accounts[saved.ClientID]
	return checkpoint.Generation == saved.Generation && checkpoint.Pending != nil && checkpoint.Pending.RevokeOnly
}
