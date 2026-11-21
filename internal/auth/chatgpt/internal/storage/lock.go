//go:build darwin || linux

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type storeLock struct {
	file   *os.File
	info   os.FileInfo
	writes chan struct{}
	closed bool
}

// WithLock serializes whole account operations, including token rotation,
// across Roast processes that share the same keyring namespace.
func (s keyringStore) WithLock(ctx context.Context, fn func(Store) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lock != nil {
		return errors.New("WithLock cannot be nested on a locked credential store")
	}
	root, err := os.Stat(filepath.Dir(s.lockPath))
	if err != nil {
		return err
	}
	stat, ok := root.Sys().(*syscall.Stat_t)
	if !ok || !root.IsDir() || stat.Uid != uint32(os.Geteuid()) || root.Mode().Perm()&0022 != 0 {
		return errors.New("the ChatGPT keyring lock requires a user-owned directory without shared write permissions")
	}
	file, err := openLock(s.lockPath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 {
		return errors.New("the ChatGPT keyring lock file must be owned by the current user and private")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A writable descriptor also works on filesystems that emulate flock with
	// record locks. Closing it releases the lock, including on process death.
	if err := waitForLock(waitCtx, file); err != nil {
		return fmt.Errorf("waiting for the ChatGPT keyring lock: %w", err)
	}
	if err := waitCtx.Err(); err != nil {
		return err
	}
	s.lock = &storeLock{file: file, info: info, writes: make(chan struct{}, 1)}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	defer func() {
		s.lock.writes <- struct{}{}
		s.lock.closed = true
		<-s.lock.writes
	}()
	// Resolve known cleanup failures before a callback can consume a refresh
	// token whose replacement would otherwise fail at the same cleanup step.
	m, err := s.readManifest(ctx)
	if err != nil {
		return err
	}
	if m.Discard.ID != "" && m.Current.ID != "" {
		// Keep the previous entries if the current snapshot is unreadable.
		data, err := s.readSnapshot(ctx, m.Current)
		if err != nil {
			return fmt.Errorf("verify the current ChatGPT credential snapshot before recovery: %w", err)
		}
		if !json.Valid([]byte(data)) {
			return errors.New("could not verify the current ChatGPT credential snapshot before recovery")
		}
	}
	if err := s.removeDiscarded(ctx, m); err != nil {
		return fmt.Errorf("recover interrupted ChatGPT credential update: %w", err)
	}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(s)
}

func openLock(path string) (*os.File, error) {
	const flags = os.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|flags, 0600)
	if errors.Is(err, os.ErrExist) {
		return os.OpenFile(path, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (l *storeLock) checkName() error {
	named, err := os.Lstat(l.file.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(l.info, named) {
		return errors.New("the ChatGPT keyring lock file was replaced")
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
