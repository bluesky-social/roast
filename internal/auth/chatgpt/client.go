// Package chatgpt owns Roast's independent Sign in with ChatGPT registrations.
package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/loopback"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

var ErrNetwork = oauth.ErrNetwork

type Client struct {
	dir      string
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
	if err := storage.CheckSupported(); err != nil {
		return nil, err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	c := &Client{
		dir:      filepath.Join(dir, "roast", "chatgpt-plan"),
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
		return accounts.save()
	})
}

// SignOut attempts revocation and clears the local grant. An empty ID uses
// the registration this client selected before waiting for the store lock.
func (c *Client) SignOut(ctx context.Context, id string) error {
	if id == "" {
		id = c.selected
	}
	if id == "" {
		return errors.New("no active ChatGPT account")
	}
	return c.withStore(ctx, func(accounts *accountStore) error {
		saved, err := accounts.credential(id)
		if err != nil {
			return err
		}

		token, tokenType := saved.revocationToken()
		var remoteErr error
		if token != "" {
			// Revocation precedes local cleanup. Bound it across discovery and
			// retries so another command can acquire the session lock.
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := c.provider.Revoke(revokeCtx, id, token, tokenType)
			cancel()
			if err != nil {
				remoteErr = errors.Join(unconfirmedRevocation(id), ctx.Err())
			}
		}

		saved.clearGrant()
		if accounts.Active == id {
			accounts.setActive("")
		}
		if err := accounts.saveCredential(&saved); err != nil {
			return errors.Join(fmt.Errorf("could not confirm local sign-out from ChatGPT account %q: %w", id, err), remoteErr)
		}
		if remoteErr != nil {
			return fmt.Errorf("ChatGPT account %q signed out locally. %w", id, remoteErr)
		}
		return nil
	})
}
