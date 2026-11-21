//go:build darwin || linux

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

type memoryKeyring struct {
	items  map[string]string
	onGet  func(string) (string, error)
	onSet  func(string, string) error
	onDrop func(string) error
}

func (m *memoryKeyring) Get(_, item string) (string, error) {
	if m.onGet != nil {
		return m.onGet(item)
	}
	return m.get(item)
}

func (m *memoryKeyring) get(item string) (string, error) {
	value, ok := m.items[item]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (m *memoryKeyring) Set(_, item, value string) error {
	if m.onSet != nil {
		return m.onSet(item, value)
	}
	m.items[item] = value
	return nil
}

func (m *memoryKeyring) Delete(_, item string) error {
	if m.onDrop != nil {
		return m.onDrop(item)
	}
	if _, ok := m.items[item]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.items, item)
	return nil
}

func testStore(t *testing.T) (keyringStore, *memoryKeyring) {
	t.Helper()
	ring := &memoryKeyring{items: make(map[string]string)}
	root := t.TempDir()
	return newStore(filepath.Join(root, "storage"), root, ring), ring
}

func save(store Store, ctx context.Context, value string) error {
	return store.WithLock(ctx, func(locked Store) error {
		return locked.WriteJSON(ctx, value)
	})
}

func assertSnapshot(t *testing.T, store Store, want string) {
	t.Helper()
	var got string
	if err := store.ReadJSON(&got); err != nil || got != want {
		t.Fatalf("unexpected credential snapshot: %v", err)
	}
}

func TestKeyringReadsDistinguishMissingAndUnavailable(t *testing.T) {
	store, ring := testStore(t)
	var got string
	if err := store.ReadJSON(&got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty keyring = %v", err)
	}
	if _, err := os.Stat(store.lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read created the lock directory: %v", err)
	}
	ring.onGet = func(string) (string, error) { return "", errors.New("private backend output") }
	if err := store.ReadJSON(&got); err == nil || errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "private backend output") {
		t.Fatalf("unavailable keyring = %v", err)
	}
}

func TestOpenReportsPreviousCredentialFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(config, "roast", "chatgpt-plan")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "accounts.json")
	if err := os.WriteFile(path, []byte("synthetic previous credentials"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(); err == nil || !strings.Contains(err.Error(), "previous Roast version") {
		t.Fatalf("opening with a previous credential file = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "synthetic previous credentials" {
		t.Fatalf("opening changed the previous credential file: %v", err)
	}
}

func TestKeyringSplitsLargeSnapshots(t *testing.T) {
	store, ring := testStore(t)
	value := strings.Repeat("é☺🔑", chunkSize)
	if err := save(store, t.Context(), value); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, value)
	otherRoot := t.TempDir()
	other := newStore(filepath.Join(otherRoot, "storage"), otherRoot, ring)
	var got string
	if err := other.ReadJSON(&got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("another namespace could read the snapshot: %v", err)
	}
	first, err := store.readManifest()
	if err != nil || first.Current.Chunks < 2 {
		t.Fatalf("snapshot did not span several entries: %v", err)
	}
	entries, err := os.ReadDir(store.lockDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("storing credentials wrote files to the lock directory: %v", err)
	}
	if err := save(store, t.Context(), "replacement"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, "replacement")
	if len(ring.items) != 2 {
		t.Fatal("retired credential entries remained after the update")
	}
}

func TestInterruptedWritePreservesCurrentSnapshot(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	ring.onSet = func(item, value string) error {
		if !strings.HasSuffix(item, ".1") {
			ring.items[item] = value
		}
		return nil // Simulate a keyring that acknowledges an unsuccessful write.
	}
	if err := save(store, t.Context(), strings.Repeat("x", 2*chunkSize)); err == nil || errors.Is(err, ErrPublished) {
		t.Fatalf("incomplete write = %v", err)
	}
	assertSnapshot(t, store, "current")
	m, err := store.readManifest()
	if err != nil || m.Discard.Chunks < 2 {
		t.Fatalf("abandoned entries were not tracked: %v", err)
	}
	ring.onSet = nil
	if err := save(store, t.Context(), "retry"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, "retry")
	if len(ring.items) != 2 {
		t.Fatal("retry left abandoned credential entries")
	}
}

func TestUncertainPublicationPreservesReplacement(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	m, err := store.readManifest()
	if err != nil {
		t.Fatal(err)
	}
	ring.onSet = func(item, value string) error {
		ring.items[item] = value
		if item == store.manifestItem() {
			var index manifest
			if err := json.Unmarshal([]byte(value), &index); err != nil {
				t.Fatal(err)
			}
			if index.Current != m.Current {
				ring.onGet = func(string) (string, error) { return "", errors.New("keyring connection lost") }
				return errors.New("keyring connection lost")
			}
		}
		return nil
	}
	if err := save(store, t.Context(), "replacement"); !errors.Is(err, ErrPublished) {
		t.Fatalf("unconfirmed commit = %v", err)
	}
	ring.onGet = nil
	assertSnapshot(t, store, "replacement")
}

func TestUnconfirmedCleanupIsRetried(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	ring.onDrop = func(string) error { return nil } // The provider did not delete the entry.
	if err := save(store, t.Context(), "replacement"); !errors.Is(err, ErrPublished) {
		t.Fatalf("unconfirmed removal = %v", err)
	}
	assertSnapshot(t, store, "replacement")
	if err := store.WithLock(t.Context(), func(Store) error {
		t.Error("account operation started with outstanding keyring cleanup")
		return nil
	}); err == nil {
		t.Fatal("accepted a keyring with outstanding cleanup")
	}
	ring.onDrop = nil
	if err := save(store, t.Context(), "next"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, "next")
	if len(ring.items) != 2 {
		t.Fatal("retry left superseded credential entries")
	}
}

func TestDeleteErrorAfterRemovingEntry(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	ring.onDrop = func(item string) error {
		delete(ring.items, item)
		return errors.New("keyring connection lost after deletion")
	}
	if err := save(store, t.Context(), "replacement"); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, store, "replacement")
	if len(ring.items) != 2 {
		t.Fatal("superseded credential entries remained")
	}
}

func TestReadRetriesIfSnapshotWasRetired(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	m, err := store.readManifest()
	if err != nil {
		t.Fatal(err)
	}
	ring.onGet = func(item string) (string, error) {
		if item == store.chunkItem(m.Current, 0) {
			ring.onGet = nil
			if err := save(store, t.Context(), "replacement"); err != nil {
				t.Fatal(err)
			}
		}
		return ring.get(item)
	}
	assertSnapshot(t, store, "replacement")
}

func TestMissingCommittedEntryIsNotAnEmptyStore(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	m, err := store.readManifest()
	if err != nil {
		t.Fatal(err)
	}
	delete(ring.items, store.chunkItem(m.Current, 0))
	var got string
	if err := store.ReadJSON(&got); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete credential snapshot = %v", err)
	}
}

func TestMalformedManifestIsNotAnEmptyStore(t *testing.T) {
	store, ring := testStore(t)
	oversized, err := json.Marshal(manifest{Discard: snapshot{ID: "INVALID", Chunks: maxChunks + 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{}", string(oversized)} {
		ring.items[store.manifestItem()] = raw
		var got string
		if err := store.ReadJSON(&got); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("malformed credential index = %v", err)
		}
	}
}

func TestInterruptedFirstWriteCleansUpBeforeRetry(t *testing.T) {
	store, ring := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ring.onSet = func(item, value string) error {
		ring.items[item] = value
		if item != store.manifestItem() {
			cancel()
		}
		return nil
	}
	if err := save(store, ctx, "abandoned"); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned update = %v", err)
	}
	ring.onSet = nil
	if err := store.WithLock(t.Context(), func(locked Store) error {
		if len(ring.items) != 0 {
			t.Error("first write left unpublished credentials in the keyring")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledWritePreservesCurrentSnapshot(t *testing.T) {
	store, ring := testStore(t)
	if err := save(store, t.Context(), "current"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ring.onSet = func(item, value string) error {
		ring.items[item] = value
		if item != store.manifestItem() {
			cancel()
		}
		return nil
	}
	if err := save(store, ctx, "replacement"); !errors.Is(err, context.Canceled) || errors.Is(err, ErrPublished) {
		t.Fatalf("canceled update = %v", err)
	}
	assertSnapshot(t, store, "current")
}
