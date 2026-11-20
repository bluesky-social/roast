//go:build darwin || linux

package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func CheckSupported() error { return nil }

func checkPrivateMode(st os.FileInfo, directory bool) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	// An open file can have zero links after an atomic replacement. Its
	// descriptor still represents the original private file.
	if !ok || stat.Uid != uint32(os.Geteuid()) || (!directory && stat.Nlink > 1) ||
		st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 ||
		(directory && !st.IsDir()) || (!directory && !st.Mode().IsRegular()) {
		return errors.New("ChatGPT storage must be owned by the current user with private permissions and no linked files")
	}
	return nil
}

const privateReadFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func checkPrivate(path string, expected os.FileInfo, directory bool) error {
	if err := checkPrivateMode(expected, directory); err != nil {
		return err
	}
	f, err := os.OpenFile(path, privateReadFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := privateFileInfo(f, directory)
	if err != nil {
		return err
	}
	if !os.SameFile(info, expected) {
		return errors.New("ChatGPT storage changed while checking permissions")
	}
	return nil
}

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

func privateFileInfo(f *os.File, directory bool) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkPrivateMode(info, directory); err != nil {
		return nil, err
	}
	return info, checkFilesystemPermissions(f)
}

func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return checkPrivatePath(dir, true)
}

func privateTemp(dir *os.File) (*os.File, error) {
	return openAt(dir, ".credentials-"+rand.Text(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
