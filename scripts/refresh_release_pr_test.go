package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshReleasePR(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is unavailable")
	}
	for _, tc := range []struct {
		name      string
		stale     string
		compare   string
		wantViews int
		wantError bool
	}{
		{"already current", "0", "ahead", 1, false},
		{"identical to base", "0", "identical", 1, false},
		{"stale metadata", "2", "ahead", 3, false},
		{"never converges", "99", "ahead", 12, true},
		{"diverged", "0", "diverged", 12, true},
		{"API failure", "0", "error", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Model the PR API returning the old head after update-branch has
			// succeeded. An up-to-date/no-op update need not change the head.
			fake := `#!/bin/bash
set -euo pipefail
case "$*" in
  'api repos/test/roast/git/ref/heads/main --jq .object.sha') echo base ;;
  'pr update-branch 7') touch "$STATE/updated"; echo 'branch updated' ;;
  'pr view 7 --json headRefOid')
    test -f "$STATE/updated"
    count=0
    if [[ -f "$STATE/views" ]]; then read -r count < "$STATE/views"; fi
    count=$((count + 1))
    echo "$count" > "$STATE/views"
    if (( count <= STALE )); then echo '{"headRefOid":"old"}'
    else echo '{"headRefOid":"new"}'; fi ;;
  'api repos/test/roast/compare/base...old --jq .status') echo behind ;;
  'api repos/test/roast/compare/base...new --jq .status')
    if [[ "$COMPARE" == error ]]; then exit 1; fi
    echo "$COMPARE" ;;
  *) echo "unexpected gh arguments: $*" >&2; exit 1 ;;
esac
`
			for name, body := range map[string]string{"gh": fake, "sleep": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", "refresh-release-pr.sh", "7", "headRefOid")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GH_REPO=test/roast", "STATE="+dir, "STALE="+tc.stale, "COMPARE="+tc.compare)
			out, err := cmd.Output()
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, output = %s", err, out)
			}
			if !tc.wantError && strings.TrimSpace(string(out)) != `{"headRefOid":"new"}` {
				t.Fatalf("returned stale metadata: %s", out)
			}
			views, err := os.ReadFile(filepath.Join(dir, "views"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(views)) != fmt.Sprint(tc.wantViews) {
				t.Fatalf("views = %s, want %d", views, tc.wantViews)
			}
		})
	}
}
