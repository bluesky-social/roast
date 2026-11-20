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

func TestLockRejectsSymlinks(t *testing.T) {
	dir := testStorageDir(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "session.lock")); err != nil {
		t.Fatal(err)
	}
	err := WithLock(t.Context(), dir, func() error {
		t.Error("accepted a symlink as a lock file")
		return nil
	})
	if err == nil {
		t.Fatal("a symlink lock must return an error")
	}
}

func TestLockReleasedOnProcessDeath(t *testing.T) {
	// The helper can only see the synthetic test store supplied by this test.
	if os.Getenv("ROAST_TEST_LOCK_HELPER") == "1" {
		dir := os.Getenv("ROAST_TEST_LOCK_DIR")
		err := WithLock(context.Background(), dir, func() error {
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
	err = WithLock(ctx, dir, func() error { return nil })
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
	if err = WithLock(ctx, dir, func() error { return nil }); err != nil {
		t.Fatalf("kernel lock survived process death: %v", err)
	}
}

func testStorageDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
