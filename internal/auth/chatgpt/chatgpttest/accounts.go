//go:build darwin || linux

package chatgpttest

import (
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/auth/chatgpt/internal/storage"
)

// SeedAccounts stores synthetic accounts in the supplied store.
func SeedAccounts(t *testing.T, store storage.Store, active string, others ...string) {
	t.Helper()
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
