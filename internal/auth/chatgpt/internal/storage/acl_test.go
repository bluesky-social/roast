//go:build darwin || linux

package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestStoreRejectsDarwinACLs(t *testing.T) {
	files := testStore(t)
	requireDarwinACL(t)
	root := t.TempDir()
	setACL(t, root, "everyone allow list,search,readattr,readsecurity,file_inherit,directory_inherit")
	err := files.ensureDir(filepath.Join(root, "store"))
	if err == nil || !strings.Contains(err.Error(), "extended ACLs") {
		t.Fatalf("inherited ACL accepted: %v", err)
	}

	path := filepath.Join(testStorageDir(t), "accounts.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	setACL(t, path, "everyone allow read")
	var data map[string]any
	if err := files.ReadJSON(path, &data); err == nil || !strings.Contains(err.Error(), "extended ACLs") {
		t.Fatalf("credentials with an extended ACL were readable: %v", err)
	}
}

func TestStoreChecksDarwinACLsBeforeWriting(t *testing.T) {
	files := testStore(t)
	requireDarwinACL(t)
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	err := files.WithLock(t.Context(), dir, func(locked Store) error {
		setACL(t, dir, "everyone allow list,search,readattr")
		return locked.WriteJSON(t.Context(), path, "synthetic credential")
	})
	if err == nil || !strings.Contains(err.Error(), "extended ACLs") {
		t.Fatalf("accepted an ACL added after acquiring the store lock: %v", err)
	}
}

func requireDarwinACL(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "ios" {
		t.Skip("requires Darwin extended ACLs")
	}
}

func setACL(t *testing.T, path, entry string) {
	t.Helper()
	if output, err := exec.Command("/bin/chmod", "+a", entry, path).CombinedOutput(); err != nil {
		t.Fatalf("set test ACL: %v (%s)", err, output)
	}
}

type ownershipFixture struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (fixture ownershipFixture) Sys() any { return &fixture.stat }

func TestUnixStorageRejectsOtherOwners(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "account.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, path} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		fixture := ownershipFixture{FileInfo: info, stat: *info.Sys().(*syscall.Stat_t)}
		if err := checkPrivateMode(fixture, info.IsDir()); err != nil {
			t.Fatalf("current owner's private storage was rejected: %v", err)
		}
		fixture.stat.Uid ^= 1
		if err := checkPrivateMode(fixture, info.IsDir()); err == nil {
			t.Fatalf("private storage owned by another user was accepted: %s", path)
		}
	}
}

func TestFirstWriteBelowSearchOnlyAncestor(t *testing.T) {
	files := testStore(t)
	if os.Geteuid() == 0 {
		t.Skip("requires Unix permission enforcement for an unprivileged user")
	}
	parent := t.TempDir()
	store := filepath.Join(parent, "store")
	if err := files.ensureDir(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	err := files.WithLock(t.Context(), store, func(locked Store) error {
		return locked.WriteJSON(t.Context(), filepath.Join(store, "accounts.json"), "synthetic account")
	})
	if err != nil {
		t.Fatalf("initial write below a search-only ancestor: %v", err)
	}
}

func TestPrivateFileInfoAcceptsUnlinkedOpenFile(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := testStore(t).privateFileInfo(f, false); err != nil {
		t.Fatalf("replacing the path invalidated the private open file: %v", err)
	}
}

func TestFilesystemPermissions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		allowed bool
	}{
		{"Linux ext4", checkLinuxMount(0xef53), true},
		{"Linux tmpfs", checkLinuxMount(0x01021994), true},
		{"Linux NFS", checkLinuxMount(0x6969), false},
		{"Linux FUSE", checkLinuxMount(0x65735546), false},
		{"Darwin APFS", checkDarwinMount("apfs", true), true},
		{"Darwin HFS+", checkDarwinMount("hfs", true), true},
		{"Darwin without local ownership", checkDarwinMount("apfs", false), false},
		{"Darwin unsupported volume", checkDarwinMount("smbfs", true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.err == nil) != tc.allowed {
				t.Fatalf("allowed = %v, error = %v", tc.allowed, tc.err)
			}
		})
	}
}
