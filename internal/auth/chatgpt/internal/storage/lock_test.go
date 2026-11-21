//go:build darwin || linux

package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	if err := os.Symlink(t.TempDir(), store.dir); err != nil {
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

func TestLockReleasedOnProcessDeath(t *testing.T) {
	if dir := os.Getenv("ROAST_TEST_LOCK_DIR"); dir != "" {
		store := newStore(dir, nil)
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
	cmd.Env = append(os.Environ(), "ROAST_TEST_LOCK_DIR="+store.dir)
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
