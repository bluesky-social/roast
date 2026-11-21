//go:build darwin || linux

package llm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/bluesky-social/roast/internal/auth/chatgpt"
)

func TestChatGPTAccountSnapshotKeepsInitializationFailure(t *testing.T) {
	// The library's mock replaces the native provider for the lifetime of the
	// process. Keep it out of the process that can run opt-in live tests.
	const child = "CHATGPT_KEYRING_TEST_CHILD"
	if os.Getenv(child) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestChatGPTAccountSnapshotKeepsInitializationFailure$")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("keyring test subprocess: %v\n%s", err, output)
		}
		return
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	keyring.MockInitWithError(errors.New("keyring unavailable in test"))
	operation := WithPinnedChatGPTAccount(t.Context())
	_, initialErr := chatGPTAccount(operation)
	if initialErr == nil {
		t.Fatal("unavailable keyring did not fail initialization")
	}
	// Repair the store without a token so a regression still fails offline.
	keyring.MockInit()
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
