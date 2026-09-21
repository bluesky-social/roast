package runbundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildIsDeterministicAcrossDirectoriesAndTimestamps(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, dir := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("metadata\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.gz"), []byte("result\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Unix(1, 0)
	newer := time.Unix(2_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(first, "meta.json"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(second, "meta.json"), newer, newer); err != nil {
		t.Fatal(err)
	}
	_, firstDigest, firstSize, err := Build(first)
	if err != nil {
		t.Fatal(err)
	}
	_, secondDigest, secondSize, err := Build(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest || firstSize != secondSize {
		t.Fatalf("bundle mismatch: %s/%d != %s/%d", firstDigest, firstSize, secondDigest, secondSize)
	}
	manifest, err := Verify(filepath.Join(first, BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != Format || manifest.Version != Version {
		t.Fatalf("manifest identity = %q v%d", manifest.Format, manifest.Version)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "meta.json" || manifest.Files[1].Path != "result.gz" {
		t.Fatalf("manifest files = %#v", manifest.Files)
	}
}

func TestBuildReusesExistingBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, digest, size, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	path2, digest2, size2, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path || digest2 != digest || size2 != size {
		t.Fatalf("cached bundle changed: %s %s %d", path2, digest2, size2)
	}
}

func TestBuildRejectsSymlinkedArtifact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("must not be bundled"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "result.json.gz")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, _, err := Build(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Build() error = %v, want symlink rejection", err)
	}
}

func TestVerifyRejectsManifestDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BundleFile)
	manifest := Manifest{
		Format: Format, Version: Version,
		Files: []ManifestEntry{{Path: "meta.json", SHA256: strings.Repeat("0", 64), Size: 4}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(path, []archiveFile{
		{name: ManifestFile, data: manifestData},
		{name: "meta.json", data: []byte("meta")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("Verify() error = %v, want digest mismatch", err)
	}
}

func TestBuildRejectsInvalidExistingBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BundleFile), []byte("not a bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Build(dir); err == nil || !strings.Contains(err.Error(), "verify existing bundle") {
		t.Fatalf("Build() error = %v, want existing bundle verification failure", err)
	}
}
