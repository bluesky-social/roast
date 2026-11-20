//go:build darwin || linux

// Package storage provides private JSON files and cross-process locks for ChatGPT credentials.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func checkPrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkPrivate(path, info, directory)
}

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

// WriteJSONContext honors cancellation up to publication of the new document.
func WriteJSONContext(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if filepath.Base(path) != "accounts.json" {
		return errors.New("invalid ChatGPT credential filename")
	}
	b, err := json.Marshal(v)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return err
	}
	dir, err := openPrivateDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := privateTemp(dir)
	if err != nil {
		return err
	}
	name := filepath.Base(f.Name())
	defer unix.Unlinkat(int(dir.Fd()), name, 0)
	// Restore owner permissions even under a restrictive umask, then check
	// inherited ACLs before writing any credentials.
	if err = f.Chmod(0600); err == nil {
		_, err = privateFileInfo(f, false)
	}
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return commitPrivate(ctx, dir, name, filepath.Base(path))
}

// CleanupTemps removes abandoned atomic-write files without reading their data.
// The caller must hold the store lock until the temporary files are removed.
func CleanupTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".credentials-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		// Never follow symlinks/reparse points or remove directories and files
		// outside the existing private-storage boundary.
		if err := checkPrivate(path, info, false); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
