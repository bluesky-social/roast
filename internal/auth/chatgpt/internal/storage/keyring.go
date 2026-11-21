//go:build darwin || linux

package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/zalando/go-keyring"
)

const (
	service = "roast-chatgpt-plan-v1"
	// Encode each chunk separately so it is valid UTF-8 on every backend.
	// On macOS, go-keyring base64-encodes it again. Leave room for both
	// encodings and the item name under the security command's 4096-byte limit.
	chunkSize = 2048
	// Allow several maximum-size OAuth replies while bounding work on a
	// corrupt index. Typical accounts need only a few entries.
	maxSnapshotSize = 16 << 20
	maxChunks       = maxSnapshotSize / chunkSize
	maxManifestSize = 512
)

type keyringAccess interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (systemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (systemKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

type keyringStore struct {
	ring      keyringAccess
	lockPath  string
	namespace string
	lock      *storeLock
}

// Open prepares a handle without accessing the vault or creating the lock directory.
func Open() (Store, error) {
	return openStore()
}

var openStore = openSystemStore

func openSystemStore() (Store, error) {
	lockRoot, err := userLockRoot()
	if err != nil {
		return nil, err
	}
	return OpenWithKeyring(systemKeyring{}, lockRoot)
}

// UseTestKeyring isolates both the operating-system keyring and the lock
// location. Tests using the library's process-wide mock must run serially.
func UseTestKeyring(lockRoot string) (restore func()) {
	keyring.MockInit()
	previous := openStore
	openStore = func() (Store, error) {
		// Use the library mock instead of the native macOS mutation path.
		return OpenWithKeyring(struct{ systemKeyring }{}, lockRoot)
	}
	return func() { openStore = previous }
}

func userLockRoot() (string, error) {
	if runtime.GOOS == "darwin" {
		// A home-directory ACL can grant access beyond its POSIX mode bits.
		// Use the system-managed local directory for the current user.
		path, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_DIR").Output()
		if err != nil {
			return "", fmt.Errorf("locate the macOS ChatGPT keyring lock directory: %w", err)
		}
		return strings.TrimSpace(string(path)), nil
	}
	account, err := user.LookupId(fmt.Sprint(os.Geteuid()))
	if err == nil && account.HomeDir != "" {
		return account.HomeDir, nil
	}
	// Some Linux containers have no passwd entry for their effective UID.
	return os.UserHomeDir()
}

// OpenWithKeyring creates a store with the given keyring client and lock root.
func OpenWithKeyring(ring keyringAccess, lockRoot string) (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(lockRoot) {
		return nil, errors.New("the ChatGPT keyring lock requires an absolute directory")
	}
	lockRoot, err = filepath.EvalSymlinks(lockRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve ChatGPT keyring lock directory: %w", err)
	}
	profile, err := resolveProfilePath(filepath.Join(dir, "roast", "chatgpt-plan"))
	if err != nil {
		return nil, fmt.Errorf("resolve ChatGPT profile directory: %w", err)
	}
	if err := checkPreviewFiles(profile); err != nil {
		return nil, err
	}
	return newStore(profile, lockRoot, ring), nil
}

// Resolve aliases of the same profile without creating any directories.
func resolveProfilePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for parent := path; ; parent = filepath.Dir(parent) {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			suffix, err := filepath.Rel(parent, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", err
		}
		// Don't fall back to hashing a dangling symlink's spelling. Once
		// its target exists, the same profile must use the resolved path.
		if _, statErr := os.Lstat(parent); !errors.Is(statErr, os.ErrNotExist) {
			return "", err
		}
	}
}

func checkPreviewFiles(profile string) error {
	entries, err := os.ReadDir(profile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check for previous ChatGPT credential files: %w", err)
	}
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		if name == "accounts.json" || (strings.HasPrefix(name, ".credentials-") && entry.Type().IsRegular()) {
			return fmt.Errorf("found a previous ChatGPT credential file at %q. Sign out with your previous Roast version or disconnect the old registration in ChatGPT Settings, then remove the old credential files and sign in again", filepath.Join(profile, entry.Name()))
		}
	}
	return nil
}

func newStore(profileDir, lockRoot string, ring keyringAccess) keyringStore {
	// Bind both paths to the keyring scope, so processes with different home
	// overrides cannot write the same entries while holding different locks.
	scope := filepath.Clean(profileDir) + "\x00" + filepath.Clean(lockRoot)
	namespace := fmt.Sprintf("%x", sha256.Sum256([]byte(scope)))
	lockPath := filepath.Join(lockRoot, ".roast-chatgpt-"+namespace+".lock")
	return keyringStore{ring: ring, lockPath: lockPath, namespace: namespace}
}

// LockPath reports the location of the credential-free coordination file.
func (s keyringStore) LockPath() string {
	return s.lockPath
}

type snapshot struct {
	ID     string `json:"id"`
	Chunks int    `json:"chunks"`
	Digest string `json:"sha256"`
}

func (v snapshot) valid() bool {
	if v.ID == "" {
		return v.Chunks == 0 && v.Digest == ""
	}
	_, digestErr := hex.DecodeString(v.Digest)
	return len(v.Digest) == sha256.Size*2 && digestErr == nil && v.Chunks > 0 && v.Chunks <= maxChunks && len(v.ID) <= 64 && strings.IndexFunc(v.ID, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '2' && r <= '7')
	}) == -1
}

// A single manifest publishes an immutable snapshot. Discard tracks either a
// staged update or the superseded snapshot, so an interrupted writer leaves
// enough information for the next writer to remove its unused entries.
type manifest struct {
	Current snapshot `json:"current"`
	Discard snapshot `json:"discard"`
}

func (s keyringStore) manifestItem() string {
	return s.namespace + ".manifest"
}

func (s keyringStore) chunkItem(v snapshot, index int) string {
	return fmt.Sprintf("%s.%s.%d", s.namespace, v.ID, index)
}

func (s keyringStore) readManifest(ctx context.Context) (manifest, error) {
	raw, err := s.getItem(ctx, s.manifestItem())
	if ctxErr := ctx.Err(); ctxErr != nil {
		return manifest{}, ctxErr
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return manifest{}, nil
	}
	if err != nil {
		return manifest{}, keyringError("read")
	}
	var m *manifest
	if len(raw) > maxManifestSize || json.Unmarshal([]byte(raw), &m) != nil || m == nil || !m.Current.valid() || !m.Discard.valid() ||
		m.Current.ID == m.Discard.ID {
		return manifest{}, errors.New("invalid ChatGPT credential index in the keyring")
	}
	// An empty snapshot is encoded explicitly. Missing or duplicate fields
	// must never be mistaken for an interrupted first write.
	canonical, err := json.Marshal(m)
	if err != nil || string(canonical) != raw {
		return manifest{}, errors.New("invalid ChatGPT credential index in the keyring")
	}
	return *m, nil
}

func (s keyringStore) ReadJSON(ctx context.Context, dst any) error {
	if s.lock != nil {
		if err := s.lock.checkName(); err != nil {
			return err
		}
	}
	for range 3 {
		m, err := s.readManifest(ctx)
		if err != nil {
			return err
		}
		if m.Current.ID == "" {
			return os.ErrNotExist
		}
		data, err := s.readSnapshot(ctx, m.Current)
		if err != nil {
			// A writer may have committed and removed the previous snapshot.
			next, readErr := s.readManifest(ctx)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if readErr == nil && next.Current != m.Current {
				continue
			}
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return errors.New("incomplete ChatGPT credential snapshot in the keyring")
		}
		if err != nil {
			return err
		}
		if json.Unmarshal([]byte(data), dst) != nil {
			return errors.New("invalid ChatGPT credential snapshot in the keyring")
		}
		return nil
	}
	return errors.New("ChatGPT credentials changed while reading the keyring; retry")
}

func (s keyringStore) getItem(ctx context.Context, item string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, err := s.ring.Get(service, item)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) && ctx.Err() == nil {
		return s.ring.Get(service, item)
	}
	return value, err
}

func (s keyringStore) readSnapshot(ctx context.Context, v snapshot) (string, error) {
	var data strings.Builder
	for i := range v.Chunks {
		raw, err := s.getItem(ctx, s.chunkItem(v, i))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return "", keyring.ErrNotFound
		}
		if err != nil {
			return "", keyringError("read")
		}
		if len(raw) > base64.RawStdEncoding.EncodedLen(chunkSize) {
			return "", errors.New("oversized ChatGPT credential entry in the keyring")
		}
		chunk, err := base64.RawStdEncoding.DecodeString(raw)
		if err != nil || len(chunk) == 0 || len(chunk) > chunkSize || (i+1 < v.Chunks && len(chunk) != chunkSize) {
			return "", errors.New("invalid ChatGPT credential entry in the keyring")
		}
		data.Write(chunk)
	}
	value := data.String()
	if fmt.Sprintf("%x", sha256.Sum256([]byte(value))) != v.Digest {
		return "", errors.New("ChatGPT credential snapshot failed its integrity check")
	}
	return value, nil
}

func (s keyringStore) WriteJSON(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.lock == nil {
		return errors.New("credential writes require the store passed to WithLock")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxSnapshotSize {
		return errors.New("ChatGPT credential snapshot exceeds the 16 MiB storage limit")
	}
	select {
	case s.lock.writes <- struct{}{}:
		defer func() { <-s.lock.writes }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.lock.closed {
		return os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	m, err := s.readManifest(ctx)
	if err != nil {
		return err
	}
	if err := s.recoverDiscarded(ctx, m); err != nil {
		return err
	}
	next := snapshot{ID: rand.Text(), Chunks: 1 + (len(data)-1)/chunkSize, Digest: fmt.Sprintf("%x", sha256.Sum256(data))}
	if _, err := s.writeManifest(ctx, manifest{Current: m.Current, Discard: next}); err != nil {
		return err
	}
	for i := range next.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := i * chunkSize
		value := base64.RawStdEncoding.EncodeToString(data[start:min(start+chunkSize, len(data))])
		if _, err := s.setVerified(ctx, s.chunkItem(next, i), value); err != nil {
			return err
		}
	}
	published := manifest{Current: next, Discard: m.Current}
	uncertain, err := s.writeManifest(ctx, published)
	if err != nil {
		if uncertain {
			return errors.Join(ErrPublished, err)
		}
		return err
	}
	if err := s.removeDiscarded(ctx, published); err != nil {
		return errors.Join(ErrPublished, err)
	}
	return nil
}

func (s keyringStore) writeManifest(ctx context.Context, m manifest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := s.lock.checkName(); err != nil {
		return false, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return false, err
	}
	uncertain, err := s.setVerified(ctx, s.manifestItem(), string(data))
	if lockErr := s.lock.checkName(); lockErr != nil {
		return true, errors.Join(err, lockErr)
	}
	return uncertain, err
}

// A provider can acknowledge a write without saving it, or fail after saving.
// Retry the same write once and use the latest readback to determine its outcome.
// The bool reports whether a failed write still might be visible.
func (s keyringStore) setVerified(ctx context.Context, item, value string) (bool, error) {
	uncertain := false
	for range 2 {
		if err := ctx.Err(); err != nil {
			return uncertain, err
		}
		if err := s.lock.checkName(); err != nil {
			return uncertain, err
		}
		setErr := s.setItem(item, value)
		got, err := s.getItem(ctx, item)
		if err == nil && got == value {
			return false, nil
		}
		uncertain = err != nil && !errors.Is(err, keyring.ErrNotFound)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return uncertain, ctxErr
		}
		if errors.Is(setErr, keyring.ErrSetDataTooBig) {
			return uncertain, errors.New("ChatGPT credential entry exceeds the system keyring size limit")
		}
	}
	return uncertain, keyringError("save")
}

func (s keyringStore) discard(ctx context.Context, v snapshot) error {
	for i := range v.Chunks {
		if err := s.deleteVerified(ctx, s.chunkItem(v, i)); err != nil {
			return err
		}
	}
	return nil
}

func (s keyringStore) removeDiscarded(ctx context.Context, m manifest) error {
	if m.Discard.ID == "" {
		return nil
	}
	if err := s.discard(ctx, m.Discard); err != nil {
		return err
	}
	if m.Current.ID != "" {
		_, err := s.writeManifest(ctx, manifest{Current: m.Current})
		return err
	}
	// An interrupted first write may leave only staging entries.
	return s.deleteVerified(ctx, s.manifestItem())
}

func (s keyringStore) recoverDiscarded(ctx context.Context, m manifest) error {
	if m.Discard.ID != "" && m.Current.ID != "" {
		data, err := s.readSnapshot(ctx, m.Current)
		if err != nil {
			return fmt.Errorf("verify the current ChatGPT credential snapshot before recovery: %w", err)
		}
		if !json.Valid([]byte(data)) {
			return errors.New("could not verify the current ChatGPT credential snapshot before recovery")
		}
	}
	return s.removeDiscarded(ctx, m)
}

func (s keyringStore) deleteVerified(ctx context.Context, item string) error {
	for range 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.lock.checkName(); err != nil {
			return err
		}
		// A provider may report an error even after deleting the item.
		_ = s.deleteItem(item)
		if _, err := s.getItem(ctx, item); errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return keyringError("confirm removal of superseded")
}

func (s keyringStore) setItem(item, value string) error {
	if _, native := s.ring.(systemKeyring); runtime.GOOS == "darwin" && native {
		// Match go-keyring's encoding without putting the value in argv.
		encoded := "go-keyring-base64:" + base64.StdEncoding.EncodeToString([]byte(value))
		line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", service, item, encoded)
		if len(line) >= 4096 {
			return keyring.ErrSetDataTooBig
		}
		cmd := exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(line)
		return s.runSecurityMutation(cmd)
	}
	return s.ring.Set(service, item, value)
}

func (s keyringStore) deleteItem(item string) error {
	if _, native := s.ring.(systemKeyring); runtime.GOOS == "darwin" && native {
		return s.runSecurityMutation(exec.Command("/usr/bin/security", "delete-generic-password", "-s", service, "-a", item))
	}
	return s.ring.Delete(service, item)
}

func (s keyringStore) runSecurityMutation(cmd *exec.Cmd) error {
	// Keep the flock until the child finishes, even if Roast exits or the
	// foreground process group receives a terminal interrupt.
	cmd.ExtraFiles = []*os.File{s.lock.file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Run()
}

// Provider errors may contain secret values. Give callers an actionable error
// without propagating the provider's raw output.
func keyringError(action string) error {
	if runtime.GOOS == "linux" {
		return fmt.Errorf("could not %s ChatGPT credentials in the system keyring; check that an unlocked Secret Service keyring is available on your user D-Bus session", action)
	}
	return fmt.Errorf("could not %s ChatGPT credentials in the system keyring; unlock the macOS Keychain and check that Roast can access it", action)
}
