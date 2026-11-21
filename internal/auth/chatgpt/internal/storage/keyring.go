//go:build darwin || linux

package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	service = "roast-chatgpt-plan-v1"
	// Encode each chunk separately so it is valid UTF-8 on every backend.
	// On macOS, go-keyring base64-encodes it again. Leave room for both
	// encodings and the item name under the security command's 4096-byte limit.
	chunkSize = 2048
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
	lockDir   string
	namespace string
	lock      *storeLock
}

// Open creates a handle without accessing the vault or creating the lock directory.
func Open() (Store, error) {
	// Use a common local root so moving the config directory or varying TMPDIR
	// cannot give two processes separate locks for the same keyring entries.
	return OpenWithKeyring(systemKeyring{}, "/tmp")
}

// OpenWithKeyring creates a store with the given keyring client and lock root.
func OpenWithKeyring(ring keyringAccess, lockRoot string) (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return newStore(filepath.Join(dir, "roast", "chatgpt-plan"), lockRoot, ring), nil
}

func newStore(profileDir, lockRoot string, ring keyringAccess) keyringStore {
	namespace := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(profileDir))))
	lockDir := filepath.Join(lockRoot, fmt.Sprintf("roast-chatgpt-%d-%s", os.Geteuid(), namespace))
	return keyringStore{ring: ring, lockDir: lockDir, namespace: namespace}
}

type snapshot struct {
	ID     string `json:"id"`
	Chunks int    `json:"chunks"`
}

func (v snapshot) valid() bool {
	if v.ID == "" {
		return v.Chunks == 0
	}
	return v.Chunks > 0 && len(v.ID) <= 64 && strings.IndexFunc(v.ID, func(r rune) bool {
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

func (s keyringStore) readManifest() (manifest, error) {
	raw, err := s.ring.Get(service, s.manifestItem())
	if errors.Is(err, keyring.ErrNotFound) {
		return manifest{}, nil
	}
	if err != nil {
		return manifest{}, keyringError("read")
	}
	var m *manifest
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil || !m.Current.valid() || !m.Discard.valid() ||
		m.Current.ID == m.Discard.ID {
		return manifest{}, errors.New("invalid ChatGPT credential index in the keyring")
	}
	return *m, nil
}

func (s keyringStore) ReadJSON(dst any) error {
	for range 3 {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Current.ID == "" {
			return os.ErrNotExist
		}
		data, err := s.readSnapshot(m.Current)
		if errors.Is(err, keyring.ErrNotFound) {
			// A writer may have committed and removed the previous snapshot.
			next, err := s.readManifest()
			if err != nil {
				return err
			}
			if next.Current != m.Current {
				continue
			}
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

func (s keyringStore) readSnapshot(v snapshot) (string, error) {
	var data strings.Builder
	for i := range v.Chunks {
		raw, err := s.ring.Get(service, s.chunkItem(v, i))
		if errors.Is(err, keyring.ErrNotFound) {
			return "", keyring.ErrNotFound
		}
		if err != nil {
			return "", keyringError("read")
		}
		chunk, err := base64.RawStdEncoding.DecodeString(raw)
		if err != nil || len(chunk) == 0 || len(chunk) > chunkSize || (i+1 < v.Chunks && len(chunk) != chunkSize) {
			return "", errors.New("invalid ChatGPT credential entry in the keyring")
		}
		data.Write(chunk)
	}
	return data.String(), nil
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
	select {
	case s.lock.writes <- struct{}{}:
		defer func() { <-s.lock.writes }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.lock.closed {
		return os.ErrClosed
	}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	m, err := s.readManifest()
	if err != nil {
		return err
	}
	if err := s.discard(ctx, m.Discard); err != nil {
		return err
	}
	next := snapshot{ID: rand.Text(), Chunks: 1 + (len(data)-1)/chunkSize}
	if _, err := s.writeManifest(ctx, manifest{Current: m.Current, Discard: next}); err != nil {
		return err
	}
	for i := range next.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := i * chunkSize
		value := base64.RawStdEncoding.EncodeToString(data[start:min(start+chunkSize, len(data))])
		if _, err := s.setVerified(s.chunkItem(next, i), value); err != nil {
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
	return s.setVerified(s.manifestItem(), string(data))
}

// A provider can acknowledge a write without saving it, or fail after saving.
// The bool reports whether a failed write still might be visible.
func (s keyringStore) setVerified(item, value string) (bool, error) {
	setErr := s.ring.Set(service, item, value)
	got, err := s.ring.Get(service, item)
	if err == nil && got == value {
		return false, nil
	}
	if errors.Is(setErr, keyring.ErrSetDataTooBig) {
		return false, errors.New("ChatGPT credential entry exceeds the system keyring size limit")
	}
	return err != nil && !errors.Is(err, keyring.ErrNotFound), keyringError("save")
}

func (s keyringStore) discard(ctx context.Context, v snapshot) error {
	for i := range v.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.deleteVerified(s.chunkItem(v, i)); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.lock.checkName(); err != nil {
		return err
	}
	return s.deleteVerified(s.manifestItem())
}

func (s keyringStore) deleteVerified(item string) error {
	err := s.ring.Delete(service, item)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return keyringError("remove superseded")
	}
	if _, err := s.ring.Get(service, item); !errors.Is(err, keyring.ErrNotFound) {
		return keyringError("confirm removal of superseded")
	}
	return nil
}

// Provider errors may contain secret values. Give callers an actionable error
// without propagating the provider's raw output.
func keyringError(action string) error {
	if runtime.GOOS == "linux" {
		return fmt.Errorf("could not %s ChatGPT credentials in the system keyring; check that an unlocked Secret Service keyring is available on your user D-Bus session", action)
	}
	return fmt.Errorf("could not %s ChatGPT credentials in the system keyring; unlock the macOS Keychain and check that Roast can access it", action)
}
