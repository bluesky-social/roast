//go:build darwin || linux

package storage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func (store localFiles) openPrivateDirectory(path string) (*os.File, error) {
	var dir *os.File
	var err error
	if store.lockedDir != nil {
		if filepath.Clean(path) != filepath.Clean(store.lockedDir.Name()) {
			return nil, errors.New("credential path is outside the locked directory")
		}
		dir, err = openAt(store.lockedDir.File, ".", privateReadFlags|unix.O_DIRECTORY, 0)
	} else {
		dir, err = os.OpenFile(path, privateReadFlags|unix.O_DIRECTORY, 0)
	}
	if err != nil {
		return nil, err
	}
	if _, err := store.privateFileInfo(dir, true); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func openAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	// Keep the descriptor live through openat, including a callback read
	// overlapping the release of the store lock.
	raw, err := dir.SyscallConn()
	if err != nil {
		return nil, err
	}
	var fd int
	var openErr error
	if err := raw.Control(func(parent uintptr) {
		fd, openErr = unix.Openat(int(parent), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: openErr}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// ReadJSON decodes one complete document from an owner-only file.
func (store localFiles) ReadJSON(path string, v any) error {
	dir, err := store.openPrivateDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := openAt(dir, filepath.Base(path), privateReadFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := store.privateFileInfo(f, false); err != nil {
		return err
	}
	decoder := json.NewDecoder(f)
	var document json.RawMessage
	readErr := decoder.Decode(&document)
	if readErr == nil {
		_, readErr = decoder.Token()
		if readErr == io.EOF && json.Unmarshal(document, v) == nil {
			return nil
		}
	}
	var fileErr *os.PathError
	if errors.As(readErr, &fileErr) {
		return fileErr
	}
	return errors.New("invalid ChatGPT credential file")
}

func privateTemp(dir *os.File) (*os.File, error) {
	return openAt(dir, ".credentials-"+rand.Text(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

// WriteJSON atomically publishes the new document while the caller holds the lock.
// It honors cancellation through the rename and returns ErrPublished if the final sync fails.
func (store localFiles) WriteJSON(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.lockedDir == nil {
		return errors.New("credential writes require the store passed to WithLock")
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
	// A custom marshaler can call back into the store. Only filesystem work
	// needs to be serialized with another writer or the release of the lock.
	select {
	case store.lockedDir.writes <- struct{}{}:
		defer func() { <-store.lockedDir.writes }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if store.lockedDir.closed {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := store.openPrivateDirectory(filepath.Dir(path))
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
		_, err = store.privateFileInfo(f, false)
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
	if err := commitPrivate(ctx, dir, name, filepath.Base(path)); err != nil {
		return err
	}
	// Keep a newly issued token in the original store even if the directory
	// moved during the exchange. A later command may open a different store.
	if err := store.lockedDir.checkNames(); err != nil {
		return fmt.Errorf("%w in the original store; check the configured directory and lock: %w", ErrPublished, err)
	}
	return nil
}

func commitPrivate(ctx context.Context, dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(fd, to, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, os.ErrNotExist) {
		// An earlier process may have created the directory without syncing
		// its parent entries. Do this under the lock before the first publish.
		if err := syncParentEntries(dir); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	if err := unix.Renameat(fd, from, fd, to); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%w but could not sync the directory: %w", ErrPublished, err)
	}
	return nil
}

func syncParentEntries(dir *os.File) error {
	var child unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &child); err != nil {
		return err
	}
	anchor := dir
	defer func() {
		if anchor != dir {
			anchor.Close()
		}
	}()
	path := ".."
	for {
		fd := int(anchor.Fd())
		var parent unix.Stat_t
		f, err := openAt(anchor, path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			// Traverse search-only ancestors if Roast could not create entries there.
			if !errors.Is(err, os.ErrPermission) {
				return err
			}
			if err := unix.Fstatat(fd, path, &parent, 0); err != nil {
				return err
			}
			if parent.Dev != child.Dev || parent.Ino == child.Ino {
				return nil
			}
			if !errors.Is(unix.Faccessat(fd, path, unix.W_OK, 0), os.ErrPermission) {
				return err
			}
			child, path = parent, path+"/.."
			continue
		}
		if anchor != dir {
			anchor.Close()
		}
		anchor, path = f, ".."
		// Compare the same directory we will sync, even across a rename.
		if err := unix.Fstat(int(anchor.Fd()), &parent); err != nil {
			return err
		}
		if parent.Dev != child.Dev || parent.Ino == child.Ino {
			return nil
		}
		child = parent
		if err := anchor.Sync(); err != nil {
			return err
		}
	}
}

// cleanupTemps removes abandoned atomic-write files without reading their data.
// The caller must hold the store lock until the temporary files are removed.
func (store localFiles) cleanupTemps(ctx context.Context, dir *os.File) error {
	for ctx.Err() == nil {
		names, readErr := dir.Readdirnames(128)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.HasPrefix(name, ".credentials-") {
				if err := store.removePrivateTemp(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	return ctx.Err()
}

func (store localFiles) removePrivateTemp(dir *os.File, name string) error {
	fd := int(dir.Fd())
	var before unix.Stat_t
	if err := unix.Fstatat(fd, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil
	}
	f, err := openAt(dir, name, privateReadFlags, 0)
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.ELOOP) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := store.privateFileInfo(f, false)
	if err != nil {
		var errno unix.Errno
		if errors.As(err, &errno) {
			return err
		}
		return nil // Leave entries that fail the owner, mode, or ACL check.
	}
	if !sameInode(&before, info) {
		return nil
	}
	var current unix.Stat_t
	if err := unix.Fstatat(fd, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameInode(&current, info) {
		return nil
	}
	return unix.Unlinkat(fd, name, 0)
}
