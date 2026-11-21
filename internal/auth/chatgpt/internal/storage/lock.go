//go:build darwin || linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

const lockFileName = "session.lock"

// WithLock runs fn with an exclusive file lock across Roast processes.
// The timeout bounds lock acquisition.
func WithLock(ctx context.Context, dir string, fn func() error) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	// Kernel-owned advisory locks are released even if Roast is killed. Never
	// unlink this file: doing so would allow separate processes to lock two inodes.
	path := filepath.Join(dir, lockFileName)
	f, err := privateLock(path)
	if err != nil {
		return err
	}
	defer f.Close()
	private, err := f.Stat()
	if err != nil {
		return err
	}
	if err := checkPrivate(path, private, false); err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock := flock.New(path, flock.SetFlag(lockOpenFlags))
	if _, err := lock.TryLockContext(waitCtx, 100*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for ChatGPT lock: %w", err)
	}
	defer lock.Unlock()
	// Flock.Stat reads the held descriptor. The lock path must still name that private file.
	held, err := lock.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(private, held) || !os.SameFile(held, current) {
		return errors.New("ChatGPT lock file changed while waiting")
	}
	if err := checkPrivate(path, held, false); err != nil {
		return err
	}
	if err := waitCtx.Err(); err != nil {
		return fmt.Errorf("waiting for ChatGPT lock: %w", err)
	}
	if deadline, ok := waitCtx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("waiting for ChatGPT lock: %w", context.DeadlineExceeded)
	}
	return fn()
}

const lockOpenFlags = os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func privateLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|lockOpenFlags, 0600)
	if errors.Is(err, os.ErrExist) {
		return os.OpenFile(path, lockOpenFlags, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return checkPrivatePath(dir, true)
}
