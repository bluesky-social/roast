// Package storage saves ChatGPT credentials in the system keyring.
package storage

import (
	"context"
	"errors"
)

// ErrPublished marks a write whose snapshot is visible or may have become
// visible. A caller must keep a grant that may already be saved.
var ErrPublished = errors.New("ChatGPT credential update may have been published")

// Store reads complete credential snapshots and serializes their updates.
type Store interface {
	ReadJSON(ctx context.Context, dst any) error
	WriteJSON(ctx context.Context, value any) error
	WithLock(ctx context.Context, fn func(Store) error) error
}

// Open prepares a handle without accessing the vault or creating the lock directory.
func Open() (Store, error) {
	if openStore == nil {
		return nil, errors.New("ChatGPT sign-in is available on macOS and Linux")
	}
	return openStore()
}

// The keyring implementation sets this on supported builds.
var openStore func() (Store, error)
