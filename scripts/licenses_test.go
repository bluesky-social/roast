package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLicenseGenerationGuards(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := os.ReadFile("licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"invalid target", "unowned directory", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			scriptDir := filepath.Join(root, "scripts")
			if err := os.Mkdir(scriptDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scriptDir, "licenses.sh"), script, 0o600); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(root, "THIRD_PARTY_LICENSES")
			target := "linux/amd64"
			if scenario == "invalid target" {
				target = "../invalid"
			} else if scenario == "symlink" {
				protected = t.TempDir()
				if err := os.Symlink(protected, filepath.Join(root, "THIRD_PARTY_LICENSES")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if err := os.Mkdir(protected, 0o700); err != nil {
				t.Fatal(err)
			}
			if scenario != "invalid target" {
				if err := os.WriteFile(filepath.Join(protected, "keep.txt"), []byte("user data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(scriptDir, "licenses.sh"), target)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("unsafe license generation was accepted")
			}
			want := "refusing to replace"
			if scenario == "invalid target" {
				want = "unsupported license target"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("wrong rejection: %s", output)
			}
			if scenario != "invalid target" {
				body, err := os.ReadFile(filepath.Join(protected, "keep.txt"))
				if err != nil || string(body) != "user data" {
					t.Fatal("license generation modified an unowned directory")
				}
			}
		})
	}
}

func TestLicenseBundleOwnershipSurvivesToolUpdates(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := os.ReadFile("licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"roast runtime notices: v1",
		"roast runtime notices: go-licenses/v2 v2.0.1",
		"roast runtime notices: unknown",
	} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "bin", "THIRD_PARTY_LICENSES"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for path, body := range map[string]string{
				"scripts/licenses.sh":                   string(script),
				"THIRD_PARTY_LICENSES/.roast-generated": marker + "\n",
				"THIRD_PARTY_LICENSES/keep.txt":         "previous bundle",
				// Stop before real tool installation. A failed generation must
				// leave the owned bundle intact, just like an unowned bundle.
				"bin/go": "#!/usr/bin/env bash\necho tool-build-reached >&2\nexit 97\n",
			} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "scripts/licenses.sh"), "linux/amd64")
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("expected simulated tool build failure")
			}
			want := "tool-build-reached"
			if marker == "roast runtime notices: unknown" {
				want = "refusing to replace"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("wrong ownership decision: %s", output)
			}
			body, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_LICENSES/keep.txt"))
			if err != nil || string(body) != "previous bundle" {
				t.Fatal("failed generation modified the previous bundle")
			}
		})
	}
}
