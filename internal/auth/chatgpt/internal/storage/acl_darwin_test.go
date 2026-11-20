package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRejectsDarwinACLs(t *testing.T) {
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

func setACL(t *testing.T, path, entry string) {
	t.Helper()
	if output, err := exec.Command("/bin/chmod", "+a", entry, path).CombinedOutput(); err != nil {
		t.Fatalf("set test ACL: %v (%s)", err, output)
	}
}
