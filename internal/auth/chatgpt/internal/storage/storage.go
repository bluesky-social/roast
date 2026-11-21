// Package storage provides private JSON files and cross-process locks for ChatGPT credentials.
package storage

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrPublished marks a write that became visible before its final directory sync failed.
var ErrPublished = errors.New("ChatGPT credentials were published")

// ReadJSON decodes an owner-only file.
func ReadJSON(path string, v any) error {
	b, err := readPrivateFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, v) != nil {
		return errors.New("invalid ChatGPT credential file")
	}
	return nil
}

// WriteJSON atomically replaces a file with an owner-only JSON document.
// Callers must hold the session lock. If syncing the directory fails after
// the rename, the new document is already visible. Callers can reread it
// under the lock before deciding whether a grant was published.
func WriteJSON(path string, v any) error {
	return WriteJSONContext(context.Background(), path, v)
}
