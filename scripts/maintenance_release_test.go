package scripts_test

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestMaintenanceReleasePolicy(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is unavailable")
	}
	const branch = "release-please--branches--main--components--roast"
	for _, scenario := range []string{
		"patch", "minor", "major", "prerelease", "same version", "older patch",
		"invalid version", "REST bot spelling", "pretend bot", "wrong author", "wrong branch", "wrong base", "fork",
		"extra file", "missing file", "manual hold", "wrong title", "missing metadata",
	} {
		t.Run(scenario, func(t *testing.T) {
			next := "1.1.2"
			pr := map[string]any{
				"author":            map[string]any{"login": "app/github-actions", "is_bot": true},
				"baseRefName":       "main",
				"headRefName":       branch,
				"isCrossRepository": false,
				"files":             []map[string]string{{"path": ".release-please-manifest.json"}, {"path": "CHANGELOG.md"}},
				"labels":            []map[string]string{},
			}
			switch scenario {
			case "minor":
				next = "1.2.0"
			case "major":
				next = "2.0.0"
			case "prerelease":
				next = "1.1.2-rc.1"
			case "same version":
				next = "1.1.1"
			case "older patch":
				next = "1.1.0"
			case "invalid version":
				next = "nonsense"
			case "wrong author":
				pr["author"] = map[string]string{"login": "contributor"}
			case "REST bot spelling":
				pr["author"] = map[string]any{"login": "github-actions[bot]", "is_bot": true}
			case "pretend bot":
				pr["author"] = map[string]any{"login": "app/github-actions", "is_bot": false}
			case "wrong branch":
				pr["headRefName"] = "feature/other"
			case "wrong base":
				pr["baseRefName"] = "other"
			case "fork":
				pr["isCrossRepository"] = true
			case "extra file":
				pr["files"] = []map[string]string{{"path": ".release-please-manifest.json"}, {"path": "CHANGELOG.md"}, {"path": "go.mod"}}
			case "missing file":
				pr["files"] = []map[string]string{{"path": "CHANGELOG.md"}}
			case "manual hold":
				pr["labels"] = []map[string]string{{"name": "release:manual"}}
			case "missing metadata":
				delete(pr, "labels")
			}
			pr["title"] = "chore(main): release " + next
			if scenario == "wrong title" {
				pr["title"] = "unrelated PR"
			}
			body, err := json.Marshal(pr)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "jq", "-e", "--arg", "base_version", "1.1.1",
				"--arg", "next_version", next, "--arg", "release_branch", branch, "-f", "maintenance-release.jq")
			cmd.Stdin = strings.NewReader(string(body))
			out, err := cmd.CombinedOutput()
			if scenario == "patch" || scenario == "REST bot spelling" {
				if err != nil || strings.TrimSpace(string(out)) != "true" {
					t.Fatalf("patch rejected: %v, %s", err, out)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || strings.TrimSpace(string(out)) != "false" {
					t.Fatalf("expected a policy rejection, not an execution error: %v, %s", err, out)
				}
			}
		})
	}
}
