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
	requireDarwinACL(t)
	root := t.TempDir()
	setACL(t, root, "everyone allow list,search,readattr,readsecurity,file_inherit,directory_inherit")
	err := EnsureDir(filepath.Join(root, "store"))
	if err == nil || !strings.Contains(err.Error(), "extended ACLs") {
		t.Fatalf("inherited ACL accepted: %v", err)
	}

	path := filepath.Join(testStorageDir(t), "accounts.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	setACL(t, path, "everyone allow read")
	var data map[string]any
	if err := ReadJSON(path, &data); err == nil || !strings.Contains(err.Error(), "extended ACLs") {
		t.Fatalf("credentials with an extended ACL were readable: %v", err)
	}
}

func TestStoreChecksDarwinACLsBeforeWriting(t *testing.T) {
	requireDarwinACL(t)
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	err := WithLock(t.Context(), dir, func() error {
		setACL(t, dir, "everyone allow list,search,readattr")
		return WriteJSON(path, "synthetic credential")
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
	if os.Geteuid() == 0 {
		t.Skip("requires Unix permission enforcement for an unprivileged user")
	}
	parent := t.TempDir()
	store := filepath.Join(parent, "store")
	if err := EnsureDir(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	err := WithLock(t.Context(), store, func() error {
		return WriteJSON(filepath.Join(store, "accounts.json"), "synthetic account")
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
	if _, err := privateFileInfo(f, false); err != nil {
		t.Fatalf("replacing the path invalidated the private open file: %v", err)
	}
}
