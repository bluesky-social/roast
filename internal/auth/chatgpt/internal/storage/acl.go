//go:build darwin || linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"

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

func checkPrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkPrivate(path, info, directory)
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

func privateFileInfo(f *os.File, directory bool) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkPrivateMode(info, directory); err != nil {
		return nil, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return info, fmt.Errorf("check ChatGPT storage filesystem: %w", err)
	}
	return info, checkFilesystemPermissions(f, &fs)
}
