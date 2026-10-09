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
go run honnef.co/go/tools/cmd/staticcheck@2026.2.1 ./...
go build ./cmd/roast
```

Format Go changes with `gofmt`. Tests use local HTTP servers and temporary Git
repositories; they must not require real model credentials or send repository
content to an inference provider.

Before opening a pull request, also check that `go mod tidy` leaves `go.mod`
and `go.sum` unchanged. CI runs vulnerability, Git-history secret, and
dependency-license checks. New runtime dependencies must use a license
compatible with Roast's MIT/Apache-2.0 dual license and should be justified in
the pull request.

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
request creates the stable tag and release, then tests and builds the release
archives with GoReleaser. GoReleaser updates `Casks/roast.rb` on `main` in
`bluesky-social/homebrew-tap` with this repository's asset URLs and checksums.
Existing `bluesky-social/tap` installations keep the same package name and
upgrade with `brew upgrade --cask roast`.

Stable patch release PRs from Release Please are queued for GitHub's native
squash auto-merge. Required CI checks and strict branch protection still apply.
The workflow refreshes stale release branches from `main` using GitHub's native
branch update, then ensures an explicit CI run exists for the resulting head.
Unchanged heads are not rebuilt hourly, and failed tests are not blindly retried.
Minor, major, prerelease, or source-changing release PRs require manual review.
Renovate supplies the dependency aging policy (three days for ordinary updates,
no delay for advisory-driven updates); release automation adds no separate timer.

The release workflow also runs hourly to reconcile bot merges whose push events
do not start another workflow with `GITHUB_TOKEN`. It runs Release Please and
the existing build/publish job directly, so no additional personal token or
GitHub App is required. Scheduled runs can be delayed by GitHub; maintainers
can run `Release Please` manually to reconcile immediately.
PR maintenance and tag publishing are independent jobs, so a PR maintenance
failure does not block publishing an already-created release.

To hold a patch release, add the `release:manual` label and disable any already
queued auto-merge (`gh pr merge <number> --disable-auto`). Automation leaves
that PR for manual review. Removing the label makes it eligible again.

The release workflow requires `TAP_GITHUB_TOKEN`, with contents write access to
the tap. It checks access before building a stable release. The archive
repository must not have this credential or publish this cask. Preview releases
never update the tap.

Validate packaging locally with GoReleaser v2.18.2:

```sh
goreleaser check
GORELEASER_CURRENT_TAG=v1.1.0 goreleaser release --snapshot --clean
```

Snapshot builds generate archives, checksums, and `dist/homebrew/Casks/roast.rb`
without publishing. Before a stable release, check its setup caveats and test
the generated cask's download strategy. Private assets use an authenticated
`gh` fallback; public assets download normally without requiring `gh`.

Homebrew upgrades replace a manually linked preview at Homebrew's `roast`
path. Run `roast --version` after upgrading to confirm which binary is active.

## Dependency notices

`bash scripts/licenses.sh` checks and collects runtime dependency licenses and
notices for all six CGO-disabled CLI release targets, plus the broker. It uses
pinned `go-licenses/v2` v2.0.1, generates per-target CSV inventories, and includes
the Go runtime/standard-library license. Generated files live in the ignored
`THIRD_PARTY_LICENSES/` directory, separate from Roast's own licenses. The script
refuses to replace a directory it did not generate.

GoReleaser runs this script before packaging, preview releases use the same
bundle, and CI checks generation for every target. The broker Dockerfile
generates notices for its Linux architecture and puts them, with Roast's own
licenses, under `/usr/share/licenses/roast/` in the final image. When changing
dependencies, inspect the generated inventories and any non-Go-code inspection
warnings; automated classification is not a substitute for legal review.

Prefer stable dependency releases. Two current exceptions were checked on
October 2, 2026:

- grpc's selected development pin is the fix identified by
  [GO-2026-6443](https://pkg.go.dev/vuln/GO-2026-6443); the latest stable 1.84.0
  still has that server-panic advisory. Roast does not call the affected server
  functions, but retain the patched pin until a fixed stable release is available.
- YAML v4 has no stable release yet. The selected release candidate is required
  by upstream JSON-schema and Anthropic SDK code. Do not replace its module with
  YAML v3: the major-version types/contracts are not interchangeable.

Revisit these exceptions when upstream releases permit it, and rerun provider
tests, module checks, vulnerability checks, and notice generation after changes.

## Security

Do not report vulnerabilities in a public issue. Follow [SECURITY.md](SECURITY.md)
instead.
