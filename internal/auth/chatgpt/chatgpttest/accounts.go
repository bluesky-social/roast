//go:build darwin || linux

package chatgpttest

import (
	"os"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// UseKeyring isolates the user directories and installs an empty test keyring.
// Tests using the keyring library's global mock must run serially.
func UseKeyring(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	t.Cleanup(storage.UseTestKeyring(root))
	return root
}

// SeedAccounts prepares an isolated test keyring and stores synthetic accounts.
func SeedAccounts(t *testing.T, active string, others ...string) {
	t.Helper()
	UseKeyring(t)
	store, err := storage.Open()
	if err != nil {
		t.Fatal(err)
	}
	accounts := make(map[string]any)
	ids := others
	if active != "" {
		ids = append([]string{active}, others...)
	}
	for _, account := range ids {
		accounts[account] = map[string]any{
			"client_id":    account,
			"subject":      account,
			"generation":   "synthetic",
			"access_token": "synthetic-" + account,
			"scope":        "resource.invoke chatgpt.tokens.use.direct",
			"expires_at":   time.Now().Add(time.Hour),
		}
	}
	snapshot := map[string]any{
		"host_id":  "urn:uuid:00000000-0000-4000-8000-000000000001",
		"active":   active,
		"accounts": accounts,
	}
	if err := store.WithLock(t.Context(), func(locked storage.Store) error {
		return locked.WriteJSON(t.Context(), snapshot)
	}); err != nil {
		t.Fatal(err)
	}
}
