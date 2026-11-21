//go:build darwin || linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

type localFiles struct {
	lockedDir  *lockedDirectory
	checkMount func(*os.File, *unix.Statfs_t) error
}

func init() {
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		platformStore = localFiles{checkMount: func(_ *os.File, fs *unix.Statfs_t) error {
			return checkLinuxMount(uint32(fs.Type))
		}}
	}
}

// Linux f_type values are fixed kernel ABI identifiers (linux/magic.h and ZFS_SUPER_MAGIC).
// These filesystems enforce Unix ownership and permission bits in the kernel.
func checkLinuxMount(kind uint32) error {
	switch kind {
	case 0xef53, // ext2/3/4
		0x58465342, // XFS
		0x9123683e, // Btrfs
		0xca451a4e, // bcachefs
		0xf2f52010, // F2FS
		0x2fc12fc1, // ZFS
		0x01021994, // tmpfs
		0x858458f6, // ramfs
		0x794c7630: // OverlayFS
		return nil
	}
	return errors.New("ChatGPT credential storage requires a supported local filesystem that enforces Unix permissions")
}

func checkDarwinMount(name string, localWithOwnership bool) error {
	if !localWithOwnership {
		return errors.New("ChatGPT credential storage requires a local filesystem with ownership enabled on macOS")
	}
	switch name {
	case "apfs", "hfs":
		return nil
	}
	return errors.New("ChatGPT credential storage requires APFS or HFS+ on macOS")
}

func sameInode(entry *unix.Stat_t, opened os.FileInfo) bool {
	st := opened.Sys().(*syscall.Stat_t)
	return entry.Dev == st.Dev && entry.Ino == st.Ino
}

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

func (store localFiles) checkPrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return store.checkPrivate(path, info, directory)
}

const privateReadFlags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func (store localFiles) checkPrivate(path string, expected os.FileInfo, directory bool) error {
	if err := checkPrivateMode(expected, directory); err != nil {
		return err
	}
	f, err := os.OpenFile(path, privateReadFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := store.privateFileInfo(f, directory)
	if err != nil {
		return err
	}
	if !os.SameFile(info, expected) {
		return errors.New("ChatGPT storage changed while checking permissions")
	}
	return nil
}

func (store localFiles) privateFileInfo(f *os.File, directory bool) (os.FileInfo, error) {
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
	return info, store.checkMount(f, &fs)
}
