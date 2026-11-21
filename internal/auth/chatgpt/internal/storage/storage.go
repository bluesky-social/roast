// Package storage provides private JSON files and cross-process locks for ChatGPT credentials.
package storage

import (
	"context"
	"errors"
)

// ErrPublished marks a write that became visible before its final directory sync failed.
var ErrPublished = errors.New("ChatGPT credentials were published")

// Store reads and updates private credential files on a supported filesystem.
type Store interface {
	ReadJSON(path string, dst any) error
	WriteJSON(ctx context.Context, path string, value any) error
	WithLock(ctx context.Context, dir string, fn func(Store) error) error
}

var platformStore Store

// Open selects the native implementation before any credential path is accessed.
func Open() (Store, error) {
	if platformStore == nil {
		return nil, errors.New("ChatGPT sign-in is available on macOS and Linux")
	}
	return platformStore, nil
}
