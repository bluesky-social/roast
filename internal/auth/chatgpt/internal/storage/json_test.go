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
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWriteJSONReservesStoreNames(t *testing.T) {
	dir := testStorageDir(t)
	err := WithLock(t.Context(), dir, func() error {
		for _, name := range []string{lockFileName, "SESSION.LOCK", ".credentials-staged"} {
			if err := WriteJSON(filepath.Join(dir, name), "test"); err == nil {
				t.Errorf("accepted reserved name %q", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCleanupInterruptedWrite(t *testing.T) {
	if os.Getenv("ROAST_TEST_INTERRUPTED_WRITE") == "1" {
		dir := os.Getenv("ROAST_TEST_LOCK_DIR")
		err := WithLock(t.Context(), dir, func() error {
			parent, err := openPrivateDirectory(dir)
			if err != nil {
				return err
			}
			defer parent.Close()
			f, err := privateTemp(parent)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := f.WriteString(`{"refresh_token":"synthetic-orphan"}`); err != nil {
				return err
			}
			if err := f.Sync(); err != nil {
				return err
			}
			fmt.Println("staged")
			time.Sleep(time.Minute) // Parent kills this process while the write is staged.
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := testStorageDir(t)
	committed := filepath.Join(dir, "accounts.json")
	if err := WriteJSON(committed, "previous credential"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupInterruptedWrite$")
	cmd.Env = append(os.Environ(), "ROAST_TEST_INTERRUPTED_WRITE=1", "ROAST_TEST_LOCK_DIR="+dir)
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
	go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "staged" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not stage a credential write")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not become ready")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // A killed helper is expected to exit unsuccessfully.
	waited = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	orphan := ""
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".credentials-") {
			orphan = filepath.Join(dir, entry.Name())
		}
	}
	if orphan == "" {
		t.Fatal("crashed writer left no temporary file to exercise cleanup")
	}
	if err := WithLock(t.Context(), dir, func() error { return CleanupTemps(dir) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan remains: %v", err)
	}
	var saved string
	if err := ReadJSON(committed, &saved); err != nil || saved != "previous credential" {
		t.Fatalf("cleanup changed committed data: %q, %v", saved, err)
	}
}

func TestCleanupWaitsForActiveWriter(t *testing.T) {
	dir := testStorageDir(t)
	staged := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- WithLock(t.Context(), dir, func() error {
			parent, err := openPrivateDirectory(dir)
			if err != nil {
				return err
			}
			defer parent.Close()
			f, err := privateTemp(parent)
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			defer f.Close()
			if _, err := f.WriteString(`"new credential"`); err != nil {
				return err
			}
			if err := f.Sync(); err != nil {
				return err
			}
			staged <- f.Name()
			<-release
			if err := f.Close(); err != nil {
				return err
			}
			return commitPrivate(t.Context(), parent, filepath.Base(f.Name()), "accounts.json")
		})
	}()
	t.Cleanup(func() { close(release); <-stopped })
	var temp string
	select {
	case temp = <-staged:
	case err := <-done:
		t.Fatalf("writer stopped before staging: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not stage data")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := WithLock(ctx, dir, func() error { return CleanupTemps(dir) })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not wait for writer: %v", err)
	}
	if _, err := os.Lstat(temp); err != nil {
		t.Fatalf("active temporary file was removed: %v", err)
	}
	// Let the writer commit before running cleanup again.
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := WithLock(t.Context(), dir, func() error { return CleanupTemps(dir) }); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := ReadJSON(filepath.Join(dir, "accounts.json"), &saved); err != nil || saved != "new credential" {
		t.Fatalf("writer lost data: %q, %v", saved, err)
	}
}

func TestCleanupLeavesUnsafeAndUnrelatedEntries(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "public file", "unrelated file"} {
		t.Run(kind, func(t *testing.T) {
			dir := testStorageDir(t)
			path := filepath.Join(dir, ".credentials-untrusted")
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					data, err := os.ReadFile(target)
					if err != nil || string(data) != "untouched" {
						t.Errorf("symlink target changed: %v", err)
					}
				})
			case "public file":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "unrelated file":
				path = filepath.Join(dir, "accounts.json")
				if err := WriteJSON(path, "untouched"); err != nil {
					t.Fatal(err)
				}
			}
			if err := WithLock(t.Context(), dir, func() error { return CleanupTemps(dir) }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("cleanup removed %s: %v", kind, err)
			}
		})
	}
}

// Cancel after encoding has begun but before the replacement becomes visible.
type cancelDuringWrite struct{ cancel context.CancelFunc }

func (v cancelDuringWrite) MarshalJSON() ([]byte, error) {
	v.cancel()
	return []byte(`"new credential"`), nil
}

func TestCanceledWriteRetainsPreviousCredential(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	if err := WithLock(t.Context(), dir, func() error {
		if err := WriteJSON(path, "previous credential"); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		return WriteJSONContext(ctx, path, cancelDuringWrite{cancel})
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("write returned %v", err)
	}
	var saved string
	if err := ReadJSON(path, &saved); err != nil || saved != "previous credential" {
		t.Fatalf("saved %q after cancellation: %v", saved, err)
	}
}

func TestCredentialFileStaysReadableWithRestrictiveUmask(t *testing.T) {
	dir := testStorageDir(t)
	path := filepath.Join(dir, "accounts.json")
	previous := syscall.Umask(0477)
	defer syscall.Umask(previous)
	err := WithLock(t.Context(), dir, func() error {
		return WriteJSON(path, "credential")
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("credential file permissions = %o, want 0600", got)
	}
	var saved string
	if err := ReadJSON(path, &saved); err != nil || saved != "credential" {
		t.Fatalf("unreadable replacement: %q, %v", saved, err)
	}
}
