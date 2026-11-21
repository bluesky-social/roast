//go:build darwin || linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Set ROAST_TEST_NATIVE_KEYRING=1 in a session with an unlocked system keyring.
func TestNativeKeyringRoundTrip(t *testing.T) {
	if os.Getenv("ROAST_TEST_NATIVE_KEYRING") != "1" {
		t.Skip("enable the native keyring test in an unlocked session")
	}
	root := privateTestHome(t)
	store := newStore(filepath.Join(root, "isolated-profile"), root, systemKeyring{})
	t.Cleanup(func() {
		ctx := context.Background()
		if err := store.WithLock(ctx, func(s Store) error {
			locked := s.(keyringStore)
			m, err := locked.readManifest()
			if err != nil {
				return err
			}
			if err := locked.discard(ctx, m.Current); err != nil {
				return err
			}
			return locked.deleteVerified(ctx, locked.manifestItem())
		}); err != nil {
			t.Error(err)
		}
	})
	data := strings.Repeat("synthetic-é☺-keyring", 240)
	if err := save(store, t.Context(), data); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, data)
	if err := save(store, t.Context(), "synthetic replacement"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, "synthetic replacement")
}
