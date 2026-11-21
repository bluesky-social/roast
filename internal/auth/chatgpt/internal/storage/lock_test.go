//go:build darwin || linux

package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteRequiresCurrentLock(t *testing.T) {
	store, _ := testStore(t)
	if err := store.WriteJSON(t.Context(), "credential"); err == nil {
		t.Fatal("write succeeded without holding the lock")
	}
	var previous Store
	if err := store.WithLock(t.Context(), func(locked Store) error {
		previous = locked
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := previous.WriteJSON(t.Context(), "credential"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after lock release = %v", err)
	}
}

func TestLockRejectsSymlinkDirectory(t *testing.T) {
	store, _ := testStore(t)
	if err := os.Symlink(t.TempDir(), store.lockDir); err != nil {
		t.Fatal(err)
	}
	err := store.WithLock(t.Context(), func(Store) error {
		t.Error("accepted a symlink as the lock directory")
		return nil
	})
	if err == nil {
		t.Fatal("a symlink lock must return an error")
	}
}

func TestReplacingConfigDirectoryKeepsKeyringLock(t *testing.T) {
	root := privateTestHome(t)
	profile := filepath.Join(root, "config")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	store := newStore(profile, root, &memoryKeyring{})
	if err := store.WithLock(t.Context(), func(Store) error {
		if err := os.Rename(profile, profile+".old"); err != nil {
			return err
		}
		if err := os.Mkdir(profile, 0700); err != nil {
			return err
		}
		contender := newStore(profile, root, store.ring)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		err := contender.WithLock(ctx, func(Store) error {
			t.Error("replacing the config directory bypassed the keyring lock")
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("waiting for the original keyring lock: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReplacingLockDuringPreflightBlocksOperation(t *testing.T) {
	store, ring := testStore(t)
	ring.onGet = func(item string) (string, error) {
		if err := os.Rename(store.lockDir, store.lockDir+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(store.lockDir, 0700); err != nil {
			t.Fatal(err)
		}
		return ring.get(item)
	}
	if err := store.WithLock(t.Context(), func(Store) error {
		t.Error("account operation started after the lock directory was replaced")
		return nil
	}); err == nil {
		t.Fatal("replacing the lock directory did not fail preflight")
	}
}

func TestLockReleasedOnProcessDeath(t *testing.T) {
	if dir := os.Getenv("ROAST_TEST_LOCK_DIR"); dir != "" {
		store := keyringStore{lockDir: dir, ring: &memoryKeyring{}}
		if err := store.WithLock(t.Context(), func(Store) error {
			fmt.Println("locked")
			time.Sleep(time.Minute)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	store, _ := testStore(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockReleasedOnProcessDeath$")
	cmd.Env = append(os.Environ(), "ROAST_TEST_LOCK_DIR="+store.lockDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "locked"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("helper failed to acquire lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not report lock acquisition")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = store.WithLock(ctx, func(Store) error {
		t.Error("contender acquired the lock held by a different process")
		return nil
	})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a different process = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := store.WithLock(ctx, func(Store) error { return nil }); err != nil {
		t.Fatalf("kernel lock survived process death: %v", err)
	}
}
