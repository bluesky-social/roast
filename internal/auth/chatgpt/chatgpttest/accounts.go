// Package chatgpttest provides synthetic ChatGPT registrations for integration tests.
package chatgpttest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// SeedAccounts seeds an isolated user config directory and returns the store path.
func SeedAccounts(t *testing.T, active string, others ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"HOME", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "roast", "chatgpt-plan")
	if err := os.MkdirAll(dir, 0700); err != nil {
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
	path := filepath.Join(dir, "accounts.json")
	if err := storage.WriteJSON(path, map[string]any{
		"host_id":  "urn:uuid:00000000-0000-4000-8000-000000000001",
		"active":   active,
		"accounts": accounts,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}
