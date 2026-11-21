//go:build darwin || linux

package llm

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestChatGPTAccountSnapshotKeepsInitializationFailure(t *testing.T) {
	storePath := testutil.SeedChatGPTAccounts(t, "account-b")
	if err := os.WriteFile(storePath, []byte("invalid state"), 0600); err != nil {
		t.Fatal(err)
	}
	operation := WithPinnedChatGPTAccount(t.Context())
	_, initialErr := chatGPTAccount(operation)
	if initialErr == nil {
		t.Fatal("invalid state did not fail initialization")
	}
	// Repair the store without a token so a regression still fails offline.
	const repaired = `{"host_id":"urn:uuid:00000000-0000-4000-8000-000000000001","active":"","accounts":{}}`
	if err := os.WriteFile(storePath, []byte(repaired), 0600); err != nil {
		t.Fatal(err)
	}
	// Derived request contexts retain the original failed selection instead of
	// silently adopting an account that appeared later in the operation.
	request, cancel := context.WithCancel(operation)
	defer cancel()
	cfg := ClientConfig{Protocol: "chatgpt", Model: "test-model", MaxOutputTokens: 100, MaxAttempts: 1}
	client, err := New(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Answer string }
	if err := client.CompleteStructured(request, "system", "user", "submit", &out); !errors.Is(err, ErrAuth) || !errors.Is(err, chatgpt.ErrAuth) || !errors.Is(err, initialErr) {
		t.Fatalf("original operation error = %v, want cached authentication failure", err)
	}
	// Starting another operation captures the new selection even if its parent
	// context carries an old snapshot. Construction does not retrieve a token.
	next := WithPinnedChatGPTAccount(operation)
	if _, err := chatGPTAccount(next); err != nil {
		t.Fatalf("next operation failed to capture the repaired store: %v", err)
	}
	client, err = New(next, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The repaired store has no selected account. Preserve both error categories
	// when the account opens successfully but cannot supply a token.
	if err := client.CompleteStructured(next, "system", "user", "submit", &out); !errors.Is(err, ErrAuth) || !errors.Is(err, chatgpt.ErrAuth) {
		t.Fatalf("missing credentials did not reach the review as an authentication error: %v", err)
	}
}
