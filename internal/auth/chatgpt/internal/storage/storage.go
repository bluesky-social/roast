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
