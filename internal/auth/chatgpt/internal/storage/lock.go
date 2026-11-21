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

	"golang.org/x/sys/unix"
)

const lockFileName = "session.lock"

type lockedDirectory struct {
	*os.File
	info     os.FileInfo
	lockInfo os.FileInfo
	writes   chan struct{}
	closed   bool
}

// WithLock runs fn with an exclusive file lock across Roast processes.
// The timeout bounds lock acquisition.
func (store localFiles) WithLock(ctx context.Context, dir string, fn func(Store) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.lockedDir != nil {
		return errors.New("WithLock cannot be nested on a locked credential store")
	}
	if dir == "" {
		return errors.New("missing ChatGPT credential directory")
	}
	dir = filepath.Clean(dir)
	if err := store.ensureDir(dir); err != nil {
		return err
	}
	// Kernel-owned advisory locks are released even if Roast is killed. Never
	// unlink this file: doing so would allow separate processes to lock two inodes.
	directory, err := store.openPrivateDirectory(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	f, err := privateLock(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := store.privateFileInfo(f, false); err != nil {
		return err
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The directory lock survives replacement of the lock-file name. Keep
	// the file lock too for compatibility with clients using that lock alone.
	if err := waitForLock(waitCtx, directory); err != nil {
		return fmt.Errorf("waiting for ChatGPT directory lock: %w", err)
	}
	if err := waitForLock(waitCtx, f); err != nil {
		return fmt.Errorf("waiting for ChatGPT lock: %w", err)
	}
	// Closing f releases the lock. After waiting, check both its permissions
	// and that the path still names the locked file.
	held, err := store.privateFileInfo(f, false)
	if err != nil {
		return err
	}
	if err := waitCtx.Err(); err != nil {
		return fmt.Errorf("waiting for ChatGPT lock: %w", err)
	}
	if deadline, ok := waitCtx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("waiting for ChatGPT lock: %w", context.DeadlineExceeded)
	}
	heldDir, err := store.privateFileInfo(directory, true)
	if err != nil {
		return err
	}
	locked := &lockedDirectory{File: directory, info: heldDir, lockInfo: held, writes: make(chan struct{}, 1)}
	if err := locked.checkNames(); err != nil {
		return err
	}
	if err := store.cleanupTemps(ctx, directory); err != nil {
		return fmt.Errorf("clean up interrupted ChatGPT credential writes: %w", err)
	}
	// Cleanup can take time. Start the account operation only if this is
	// still the directory another Roast process would lock.
	if err := locked.checkNames(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store.lockedDir = locked
	// Finish admitted writes before releasing the file lock. A retained
	// callback store must not publish after a later writer acquires it.
	defer func() {
		locked.writes <- struct{}{}
		locked.closed = true
		<-locked.writes
	}()
	return fn(store)
}

func (dir *lockedDirectory) checkNames() error {
	named, err := os.Lstat(dir.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(dir.info, named) {
		return errors.New("the configured ChatGPT credential directory was replaced")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), lockFileName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameInode(&current, dir.lockInfo) {
		return errors.New("the ChatGPT credential lock was replaced")
	}
	return nil
}

func waitForLock(ctx context.Context, file *os.File) error {
	for ctx.Err() == nil {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch err {
		case unix.EINTR:
			continue
		case unix.EWOULDBLOCK:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		default:
			return err
		}
	}
	return ctx.Err()
}

const lockOpenFlags = os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func privateLock(directory *os.File) (*os.File, error) {
	f, err := openAt(directory, lockFileName, os.O_CREATE|os.O_EXCL|lockOpenFlags, 0600)
	if errors.Is(err, os.ErrExist) {
		return openAt(directory, lockFileName, lockOpenFlags, 0)
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

func (store localFiles) ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return store.checkPrivatePath(dir, true)
}
