# Contributing

Thanks for helping improve Roast. Bug reports and focused pull requests are
welcome. For large features, new dependencies, inference protocols, or changes
to artifact formats, open an issue before writing the implementation so the
contract can be agreed on first.

## Development

Roast requires Go 1.27.2 or newer. Clone the repository, then run:

```sh
go test ./...
go vet ./...
go tool -modfile=.github/tools/staticcheck/go.mod staticcheck ./...
go build ./cmd/roast
```

CI-only Go tools have separate modules under `.github/tools/`, maintained by
Renovate without adding them to Roast's runtime dependencies. Use those modules
locally too; a separately installed version can have a different Go importer.
Run the workflow and shell checks with:

```sh
go tool -modfile=.github/tools/actionlint/go.mod actionlint
shellcheck scripts/*.sh
goreleaser check
```

Format Go changes with `gofmt`. Tests use local HTTP servers and temporary Git
repositories; they must not require real model credentials or send repository
content to an inference provider.

Before opening a pull request, also check that `go mod tidy` leaves `go.mod`
and `go.sum` unchanged, including the tool modules (run `go mod tidy` inside each
tool directory). CI runs vulnerability, Git-history secret, and
dependency-license checks. New runtime dependencies must use a license
compatible with Roast's MIT/Apache-2.0 dual license and should be justified in
the pull request.

Pull-request CI runs on GitHub-hosted runners with read-only repository
permissions and no inference, upload, or tap credentials. Keep that boundary:
do not run contributor-controlled code in `pull_request_target` or a privileged
`workflow_run` job. Pin external actions to full commit SHAs. Go caches use the
standard setup-go branch scopes, and container caches use BuildKit's GitHub
Actions backend. Do not bootstrap privileged jobs from pull-request artifacts.

Keep changes narrow and include regression tests for behavior changes. Update
the README whenever a public flag, environment variable, configuration field,
artifact, data destination, or exit-code behavior changes.

## Preview releases

Maintainers can manually run the `Preview Release` GitHub Actions workflow for
the revision selected in the workflow UI. It creates a GitHub prerelease with
a non-production tag and a `roast-preview` executable, without updating
Homebrew or the stable release channel. Preview visibility follows repository
visibility.

Download and unpack the archive for your platform with `gh release download`,
then run `roast-preview`. It reads the normal Roast configuration but can live
beside an installed `roast`. To exercise a pre-commit gate with the preview
binary, set `ROAST_BIN=/absolute/path/to/roast-preview` for the Git operation.

## Stable releases and Homebrew

Release Please maintains a release pull request on `main`. Merging that pull
request creates the stable tag and a **draft** release. GoReleaser reuses that
draft, preserves Release Please's notes, and publishes it only after uploading
the release artifacts. An unfinished draft is not returned by GitHub's latest
release API, so Roast does not advertise an upgrade before downloads are ready.
GoReleaser updates `Casks/roast.rb` on `main` in
`bluesky-social/homebrew-tap` with this repository's asset URLs and checksums.
Existing `bluesky-social/tap` installations keep the same package name and
upgrade with `brew upgrade --cask roast`.

Release automation is the org reusable workflow
(`bluesky-social/.github` `.github/workflows/release-please.yml`; commit
rules are in the README's "Releases and commit messages"). The
`bsky-release-bot` App maintains the Release PR, so its CI runs normally. A
Release PR auto-merges only when every commit since the last tag came from a
merged Renovate or Dependabot PR made of that bot's own verified commits, and
only from the hourly scheduled run on weekdays 09:00-16:00 PT. Required CI
checks and strict branch protection still apply. Any human-authored change,
including a `fix:`, waits for a maintainer to merge the Release PR. Renovate
supplies the dependency aging policy (three days for ordinary updates, no
delay for advisory-driven updates). Maintainers can run `Release Please`
manually to queue an eligible release immediately or reconcile a missed tag.
Once a release is cut, PR-maintenance and gate failures in the same run do
not block the binaries job.

If release building or uploading fails, fix the cause and use GitHub's **Re-run
failed jobs** on the original run. That retains the original release outputs
and reuses the draft. Once the release is published, draft lookup is no longer
the right retry mode. To repair a failed tap update, rebuild the same tag with
GoReleaser's default `release.use_existing_draft: false` in a temporary local
configuration, preserving `release.mode: keep-existing`. Do not create or retag
a new version just to repair an upload, or commit that retry-only configuration.

To hold a patch release, add the `release:manual` label and disable any already
queued auto-merge (`gh pr merge <number> --disable-auto`). Automation leaves
that PR for manual review. Removing the label makes it eligible again.

The release workflow requires `TAP_GITHUB_TOKEN`, with contents write access to
the tap. It checks access before building a stable release. The archive
repository must not have this credential or publish this cask. Preview releases
never update the tap.

Validate packaging locally with the GoReleaser version pinned in the workflows:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

Snapshot builds generate `roast-preview` archives and checksums without
publishing. The preview workflow uses this same packaging configuration, never
updates Homebrew, and publishes only a prerelease. Stable builds retain the
`roast` executable and cask identity. Before a stable release, check its setup
caveats and test the generated cask's download strategy. Private assets use an
authenticated `gh` fallback; public assets download normally without requiring
`gh`.

Build metadata, binary modification times, and archived file timestamps derive
from the commit, not the build clock. To compare rebuilt archives, use the same
commit, configuration, toolchain, and snapshot version (set
`ROAST_PREVIEW_VERSION=preview-local` for a fixed preview version).

Homebrew upgrades replace a manually linked preview at Homebrew's `roast`
path. Run `roast --version` after upgrading to confirm which binary is active.

## Dependency notices

`bash scripts/licenses.sh` checks and collects runtime dependency licenses and
notices for all six CGO-disabled CLI release targets, plus the broker. It uses
the `go-licenses/v2` tool module in `.github/tools/licenses`, generates per-target
CSV inventories, and includes the Go runtime/standard-library license.
It also extracts the embedded Igneous Systems MIT grant from the selected
`md5-simd` assembly source into `NOTICE-Igneous-Systems-MIT.txt`; the scanner only
collects its root Apache license. Generation fails if the expected complete
grant cannot be found, leaving any previous generated bundle intact.
Generated files live in the ignored
`THIRD_PARTY_LICENSES/` directory, separate from Roast's own licenses. The script
refuses to replace a directory it did not generate.

The ownership marker identifies the bundle format, not the scanner version, so
tool upgrades do not invalidate a previously generated bundle.

GoReleaser runs this script before packaging, preview releases use the same
bundle, and CI checks generation for every target. The broker Dockerfile
generates notices for its Linux architecture and puts them, with Roast's own
licenses, under `/usr/share/licenses/roast/` in the final image. When changing
dependencies, inspect the generated inventories and any non-Go-code inspection
warnings; automated classification is not a substitute for legal review.

Prefer tagged stable dependency releases. Explain any necessary development pin
or prerelease in its pull request and revisit it when a stable release permits.
Rerun provider tests, module checks, vulnerability checks, and notice generation
after dependency changes; old advisory notes are not a substitute for scanning
the selected module graph.

## Security

Do not report vulnerabilities in a public issue. Follow [SECURITY.md](SECURITY.md)
instead.
