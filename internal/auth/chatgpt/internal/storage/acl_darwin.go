package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACL entries can grant access independently of the Unix mode bits.
func checkFilesystemPermissions(f *os.File) error {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return fmt.Errorf("check ChatGPT storage filesystem: %w", err)
	}
	if fs.Flags&(unix.MNT_LOCAL|unix.MNT_IGNORE_OWNERSHIP) != unix.MNT_LOCAL {
		return errors.New("ChatGPT credential storage requires a local filesystem with ownership enabled on macOS")
	}
	switch unix.ByteSliceToString(fs.Fstypename[:]) {
	case "apfs", "hfs":
	default:
		return errors.New("ChatGPT credential storage requires APFS or HFS+ on macOS")
	}
	attrs := unix.Attrlist{
		Bitmapcount: 5,
		Commonattr:  unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_EXTENDED_SECURITY,
	}
	// The reply starts with its length, the returned attribute bitmap, and
	// the ACL reference. The ACL's length is enough to detect an extended ACL.
	var reply [32]byte
	//lint:ignore SA1019 x/sys has no fgetattrlist wrapper and Roast builds without cgo.
	_, _, errno := syscall.Syscall6(unix.SYS_FGETATTRLIST, f.Fd(),
		uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)),
		0, 0)
	if errno != 0 {
		return fmt.Errorf("check ChatGPT storage ACL: %w", errno)
	}
	length := binary.NativeEndian.Uint32(reply[:4])
	returned := binary.NativeEndian.Uint32(reply[4:8])
	if length < 24 || returned&unix.ATTR_CMN_RETURNED_ATTRS == 0 {
		return errors.New("could not check ChatGPT storage ACL")
	}
	// APFS leaves this attribute out for files without an ACL.
	if returned&unix.ATTR_CMN_EXTENDED_SECURITY == 0 {
		return nil
	}
	if length < uint32(len(reply)) {
		return errors.New("could not check ChatGPT storage ACL reference")
	}
	if binary.NativeEndian.Uint32(reply[28:32]) != 0 {
		return errors.New("ChatGPT storage must have owner-only permissions without extended ACLs")
	}
	return nil
}
