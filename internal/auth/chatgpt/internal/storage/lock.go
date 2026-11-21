//go:build darwin || linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type storeLock struct {
	dir    *os.File
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
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	dir, err := os.OpenFile(s.dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("the ChatGPT keyring lock directory must be owned by the current user and private")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Lock the directory itself so replacing a lock file cannot split the lock.
	// Closing the directory releases the lock, including on process death.
	if err := waitForLock(waitCtx, dir); err != nil {
		return fmt.Errorf("waiting for the ChatGPT keyring lock: %w", err)
	}
	if err := waitCtx.Err(); err != nil {
		return err
	}
	s.lock = &storeLock{dir: dir, info: info, writes: make(chan struct{}, 1)}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	defer func() {
		s.lock.writes <- struct{}{}
		s.lock.closed = true
		<-s.lock.writes
	}()
	return fn(s)
}

func (l *storeLock) checkName() error {
	named, err := os.Lstat(l.dir.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(l.info, named) {
		return errors.New("the ChatGPT keyring lock directory was replaced")
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
