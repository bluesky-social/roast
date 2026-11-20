package storage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func checkFilesystemPermissions(f *os.File) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return fmt.Errorf("check ChatGPT storage filesystem: %w", err)
	}
	// These filesystems enforce Unix ownership and permission bits in the
	// kernel. Mounts that can synthesize private modes are excluded.
	const zfsSuperMagic = 0x2fc12fc1
	switch uint32(fs.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC,
		unix.BCACHEFS_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC, zfsSuperMagic,
		unix.TMPFS_MAGIC, unix.RAMFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return nil
	default:
		return errors.New("ChatGPT credential storage requires a supported local filesystem that enforces Unix permissions")
	}
}
