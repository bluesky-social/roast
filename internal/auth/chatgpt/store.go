package chatgpt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/oauth"
	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

type accountStore struct {
	storage   storage.Store
	HostID    string                `json:"host_id"`
	Active    string                `json:"active"`
	Selection string                `json:"selection_version,omitempty"`
	Accounts  map[string]credential `json:"accounts"`
}

func (c *Client) loadStore(ctx context.Context) (*accountStore, error) {
	return loadAccountStore(ctx, c.store)
}

func loadAccountStore(ctx context.Context, store storage.Store) (*accountStore, error) {
	s := &accountStore{storage: store}
	err := store.ReadJSON(ctx, s)
	if errors.Is(err, os.ErrNotExist) {
		s.Accounts = make(map[string]credential)
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if s.HostID == "" || s.Accounts == nil {
		return nil, errors.New("invalid ChatGPT account store")
	}
	if _, ok := s.Accounts[s.Active]; s.Active != "" && !ok {
		return nil, errors.New("active ChatGPT registration is missing from the account store")
	}
	for id, saved := range s.Accounts {
		if saved.PendingRegistration && (id == s.Active || saved.Subject != "" || saved.Email != "" ||
			saved.IDToken != "" || saved.AccessToken != "" || saved.RefreshToken != "" || (saved.Pending != nil && !saved.Pending.RevokeOnly) ||
			saved.Scopes != "" || !saved.ExpiresAt.IsZero() || saved.EarliestRefreshAt != 0) {
			return nil, errors.New("pending ChatGPT registration is active or contains identity or grant data")
		}
		if !ValidAccountID(id) || saved.ClientID != id || saved.Generation == "" {
			return nil, errors.New("invalid ChatGPT registration in the account store")
		}
	}
	return s, nil
}

// withStore serializes updates and token rotation across Roast processes.
func (c *Client) withStore(ctx context.Context, fn func(*accountStore) error) error {
	return c.store.WithLock(ctx, func(locked storage.Store) error {
		s, err := loadAccountStore(ctx, locked)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(s)
	})
}

func (s *accountStore) save(ctx context.Context) error {
	return s.storage.WriteJSON(ctx, s)
}

// Track explicit selections even when the active account stays the same.
func (s *accountStore) setActive(id string) {
	s.Active = id
	s.Selection = rand.Text()
}

func (s *accountStore) credential(id string) (credential, error) {
	saved, ok := s.Accounts[id]
	if !ok {
		return credential{}, fmt.Errorf("ChatGPT registration %q: %w", id, os.ErrNotExist)
	}
	return saved, nil
}

// Every grant update invalidates an authorization for the earlier grant.
func (s *accountStore) saveCredential(saved *credential) error {
	return s.saveCredentialContext(context.Background(), saved)
}

func (s *accountStore) saveCredentialContext(ctx context.Context, saved *credential) error {
	saved.Generation = rand.Text()
	s.Accounts[saved.ClientID] = *saved
	return s.save(ctx)
}

func (s *accountStore) ensureHost(ctx context.Context) error {
	if s.HostID != "" {
		return nil
	}
	s.HostID = oauth.NewHostID()
	return s.save(ctx)
}
