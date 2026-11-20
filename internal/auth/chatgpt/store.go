package chatgpt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

type accountStore struct {
	path      string
	HostID    string                `json:"host_id"`
	Active    string                `json:"active"`
	Selection string                `json:"selection_version,omitempty"`
	Accounts  map[string]credential `json:"accounts"`
}

func (c *Client) storePath() string {
	return filepath.Join(c.dir, "accounts.json")
}

func (c *Client) loadStore() (*accountStore, error) {
	s := &accountStore{path: c.storePath()}
	err := storage.ReadJSON(s.path, s)
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
		if saved.PendingRegistration && (saved.Subject != "" || saved.IDToken != "" || saved.AccessToken != "" || saved.RefreshToken != "" || saved.Pending != nil) {
			return nil, errors.New("pending ChatGPT registration contains credentials")
		}
		if !ValidAccountID(id) || saved.ClientID != id || saved.Generation == "" {
			return nil, errors.New("invalid ChatGPT registration in the account store")
		}
	}
	return s, nil
}

// withStore serializes updates and token rotation across Roast processes.
func (c *Client) withStore(ctx context.Context, fn func(*accountStore) error) error {
	return storage.WithLock(ctx, c.dir, func() error {
		if err := storage.CleanupTemps(c.dir); err != nil {
			return fmt.Errorf("clean up interrupted ChatGPT credential writes: %w", err)
		}
		s, err := c.loadStore()
		if err != nil {
			return err
		}
		return fn(s)
	})
}

func (s *accountStore) save() error {
	return storage.WriteJSON(s.path, s)
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
	return storage.WriteJSONContext(ctx, s.path, s)
}

func (s *accountStore) ensureHost() error {
	if s.HostID != "" {
		return nil
	}
	s.HostID = newHostID()
	return s.save()
}

// The provider expects a version 4 UUID in URN form.
func newHostID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
