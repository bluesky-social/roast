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
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func openPrivateDirectory(path string) (*os.File, error) {
	dir, err := os.OpenFile(path, privateReadFlags|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	if _, err := privateFileInfo(dir, true); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func openAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readPrivateFile(path string) ([]byte, error) {
	dir, err := openPrivateDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := openAt(dir, filepath.Base(path), privateReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := privateFileInfo(f, false); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func privateTemp(dir *os.File) (*os.File, error) {
	return openAt(dir, ".credentials-"+rand.Text(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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

func commitPrivate(ctx context.Context, dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(fd, to, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, os.ErrNotExist) {
		// An earlier process may have created the directory without syncing
		// its parent entries. Do this under the lock before the first publish.
		if err := syncParentEntries(dir.Name()); err != nil {
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

func syncParentEntries(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	child, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return err
	}
	info, err := os.Stat(child)
	if err != nil {
		return err
	}
	device := info.Sys().(*syscall.Stat_t).Dev
	for parent := filepath.Dir(child); parent != child; parent = filepath.Dir(child) {
		info, err := os.Stat(parent)
		if err != nil {
			return err
		}
		// The enclosing filesystem cannot persist entries on this volume.
		if info.Sys().(*syscall.Stat_t).Dev != device {
			break
		}
		if err := syncDirectory(parent); err != nil {
			// A search-only ancestor can be traversable but unreadable.
			// Skip it only if Roast could not create entries there.
			const writeAccess = 2
			if !errors.Is(err, os.ErrPermission) || !errors.Is(syscall.Access(parent, writeAccess), os.ErrPermission) {
				return err
			}
		}
		child = parent
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
