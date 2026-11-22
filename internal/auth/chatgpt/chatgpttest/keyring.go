//go:build darwin || linux

// Package chatgpttest provides fixtures for testing ChatGPT account operations.
package chatgpttest

import (
	"os"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// NewStore opens a credential store backed by a fresh in-memory keyring.
func NewStore(t *testing.T, root string) (storage.Store, *Keyring) {
	t.Helper()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ring := &Keyring{items: make(map[string]string)}
	store, err := storage.OpenWithKeyring(ring, root, root)
	if err != nil {
		t.Fatal(err)
	}
	return store, ring
}

// Keyring is an in-memory keyring for a single test fixture.
type Keyring struct {
	mu    sync.Mutex
	items map[string]string
	err   error
}

// SetError makes keyring operations fail until the error is cleared.
func (k *Keyring) SetError(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *Keyring) Get(_, item string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return "", k.err
	}
	if value, ok := k.items[item]; ok {
		return value, nil
	}
	return "", keyring.ErrNotFound
}

func (k *Keyring) Set(_, item, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return k.err
	}
	k.items[item] = value
	return nil
}

func (k *Keyring) Delete(_, item string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.err != nil {
		return k.err
	}
	if _, ok := k.items[item]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.items, item)
	return nil
}
