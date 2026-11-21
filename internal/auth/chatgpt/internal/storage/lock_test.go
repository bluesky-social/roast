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

	"golang.org/x/sys/unix"
)

func TestCanceledCleanupDoesNotStartAccountOperation(t *testing.T) {
	dir := testStorageDir(t)
	orphan := filepath.Join(dir, ".credentials-orphan")
	if err := os.WriteFile(orphan, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	files := testStore(t)
	checkMount := files.checkMount
	files.checkMount = func(f *os.File, stat *unix.Statfs_t) error {
		if f.Name() == orphan {
			cancel()
		}
		return checkMount(f, stat)
	}
	err := files.WithLock(ctx, dir, func(Store) error {
		t.Error("started the account operation after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithLock returned %v, want cancellation", err)
	}
}

func TestLockedWriteStaysWithItsDirectory(t *testing.T) {
	files := testStore(t)
	dir := testStorageDir(t)
	moved := dir + "-moved"
	path := filepath.Join(dir, "accounts.json")
	err := files.WithLock(t.Context(), dir+string(os.PathSeparator), func(locked Store) error {
		if err := os.Rename(dir, moved); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		return locked.WriteJSON(t.Context(), path, "original")
	})
	if !errors.Is(err, ErrPublished) {
		t.Fatalf("lost publication or reported success after the directory moved: %v", err)
	}
	var value string
	if err := files.ReadJSON(filepath.Join(moved, "accounts.json"), &value); err != nil || value != "original" {
		t.Fatalf("locked write missed original directory: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locked write reached replacement directory: %v", err)
	}
}

func TestLockRejectsSymlinks(t *testing.T) {
	dir := testStorageDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "session.lock")); err != nil {
		t.Fatal(err)
	}
	err := testStore(t).WithLock(t.Context(), dir, func(Store) error {
		t.Error("accepted a symlink as a lock file")
		return nil
	})
	if err == nil {
		t.Fatal("a symlink lock must return an error")
	}
}

func TestLockReleasedOnProcessDeath(t *testing.T) {
	files := testStore(t)
	// The helper can only see the synthetic test store supplied by this test.
	if os.Getenv("ROAST_TEST_LOCK_HELPER") == "1" {
		dir := os.Getenv("ROAST_TEST_LOCK_DIR")
		err := files.WithLock(context.Background(), dir, func(Store) error {
			fmt.Println("locked")
			time.Sleep(time.Minute)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := testStorageDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedOnProcessDeath$")
	cmd.Env = append(os.Environ(), "ROAST_TEST_LOCK_HELPER=1", "ROAST_TEST_LOCK_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = files.WithLock(ctx, dir, func(Store) error { return nil })
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("child process did not exclude parent: %v", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = files.WithLock(ctx, dir, func(Store) error { return nil }); err != nil {
		t.Fatalf("kernel lock survived process death: %v", err)
	}
}

func testStorageDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testStore(t *testing.T) localFiles {
	t.Helper()
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return s.(localFiles)
}
