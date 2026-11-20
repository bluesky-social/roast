//go:build darwin || linux

package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

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
