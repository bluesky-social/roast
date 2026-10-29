# Open-source migration journal

Audit checkpoint: October 1, 2026; authorized fixes: October 2, 2026.
This is the single temporary journal and
checklist for the migration. The earlier `OPEN_SOURCE.md` is not present in
either current checkout or its tracked history; this document reconstructs the
decisions and records what this audit verified, not the original journal text.
Review and remove or condense it during the final publication preparation.

## Boundaries

- Keep `roast` and `roast-archive` private until explicitly authorized otherwise.
- Do not do the final squash/rebase, force-push, or change visibility.
- The initial audit/preparation phase held commits locally and did not publish
  a stable release or replace the installed preview.
- October 2: fix audit items 1-5 as authorized. Eval remains intentionally
  deferred. Subsequent authorization explicitly permits committing/pushing,
  cutting a new stable release, and testing the Homebrew installation locally.

## Verified migration decisions

- [x] Use the Bluesky public-project MIT/Apache-2.0 license shape. Roast's own
  license texts are in the repository and CLI release archives.
- [x] Remove the platform-repository dependency. Needed local behavior now uses
  this repository's implementation or standard libraries.
- [x] Replace the hand-written inference adapters and agent loop with Fantasy.
  Eino is no longer in the selected module graph. Roast still owns its review
  policy, grounding, run artifacts, and resource limits.
- [x] Support `chat`, `responses`, and native `anthropic`, including user-supplied
  endpoints and credential sources. This is three supported wire contracts,
  not a claim that every Fantasy provider is exposed.
- [x] Use named inference profiles and finder author routes. A route selects
  the entire endpoint/model/protocol/credential profile, not only a model name.
- [x] Preserve configurable code-finder, prose-finder, and verifier prompts.
- [x] Preserve local review modes, repository survey, language packs, staged
  tree pinning, grounding, verification, output formats, and exit codes.
  `--no-verify` is present; it was not lost in the migration.
- [x] Preserve durable records, snapshots, `runs show|checkout|prune`, and
  upload `runs status|sync`.
- [x] Preserve automatic uploads after completed runs when explicitly enabled.
  Uploads default to disabled. Direct uploads use S3-compatible endpoints and
  explicit credential environment variables, not an AWS credential chain.
- [x] Ship the generic presigned broker source and Dockerfile. Configuration
  URLs provide organization policy without embedding internal infrastructure
  in Roast. Broker deployment still requires authentication at its proxy.
- [x] Restore `doctor`, `skill install|status`, and `gate enable|status|disable`.
  Interactive gate enrollment asks once per repository and records declines;
  non-interactive runs do not install a hook.
- [x] Replace automatic self-updating with a cached, informational stable
  release check. No hidden update helper command is required.
- [x] Add CI, dependency-update automation, private preview builds, release
  preparation, and public contributor/security documentation.

### Deliberate archive exclusions

These are not missing functionality to copy wholesale:

| Archive area | Disposition |
| --- | --- |
| `internal/diderot`, `internal/skipper`, `internal/sourcebot`, external search | Excluded. Review tools navigate local repository state only. |
| Telemetry | Removed, including Roast-owned telemetry emission. Indirect provider SDK dependencies are not evidence of restored Roast telemetry. |
| `internal/install`, `internal/surface` | Replace skill/gate setup with current commands. Do not restore the Claude PostToolUse findings hook. Old-install cleanup is still needed below. |
| `cmd/aigw-probe` | Internal hard-coded gateway smoke test. Current `doctor` provides configured-provider diagnostics; do not copy the probe. |
| `internal/eval`, `internal/label`, replay commands | Deferred to a separate `roast-eval`, outside the main binary and Homebrew package. |
| Internal corpora, prices, plans, handoffs, model menu, login | Excluded. Initialize with user profiles or a trusted configuration URL instead. |

No additional unintended missing user-facing subcommand was identified in this
pass. Command-tree tests reject the intentionally excluded commands, including
`runs label`. Some dormant branch-survey helpers remain in `internal/scope`,
but neither CLI exposes branch scope or `--include-worktree`; this is optional
cleanup, not a lost public command.

## Follow-ups found by this audit

### 1. Legacy configuration migration blocker

- [x] Accept and deliberately discard retired legacy config sections during
  migration while retaining strict validation for current version-1 configs.
- [x] Test both ordinary config loading and configuration-URL provisioning
  against representative archive configs, including `gate` and `sourcebot`.

The original audit found that `legacyConfig` omitted the archive's `gate` and
`sourcebot` sections, breaking strict loading and provisioning. It now recognizes
those retired fields only in the legacy schema and discards them during
migration. Unknown legacy keys and retired fields in version-1 configs still
fail validation. Credential-only legacy configs are also recognized.

Previously reproduced with an inline-role config containing dummy credentials and
`example.invalid` URLs, via `go run ./cmd/roast doctor --config <fixture> --json`:

```text
field gate not found in type config.legacyConfig
field sourcebot not found in type config.legacyConfig
```

Regression tests cover loading, in-place provisioning/backups, discarded
retrieval credentials, and preservation of the opted-in gateway credential.
No retired retrieval functionality was restored.

### 2. Useful prompt regression tests were not migrated

- [x] Adapt the 22 archive `internal/prompts/prompts_test.go` tests covering
  mode/scope assembly, language-pack deduplication, quick behavior, overrides,
  verifier refutation, anti-anchoring, and insufficient-context safeguards.
  Keep obsolete retrieval and unsupported branch-scope expectations excluded.

All 22 adapted tests pass. Porting exposed a stale verifier instruction claiming
`list_files` always reads the live working tree. The prompt and regression now
defer to each tool's captured-corpus disclosure. No branch scope or retrieval
expectations were restored.

### 3. Dependency notices and distribution review

- [x] Generate/package applicable dependency licenses and notices for stable
  CLI archives, preview archives, and broker images, using pinned
  `go-licenses/v2` v2.0.1 and the release-target build graphs.
- [ ] Final human/legal review of attribution and non-Go-code inspection
  warnings, alongside the rest of the publication content review.
- [ ] Before flipping the repository public, confirm the Claude review/mention
  jobs skip: they require the org variable `AGENT_GATEWAY_URL`, which only
  private and internal repositories can read. No internal endpoint should
  remain anywhere under `.github/`.
- [x] Validate final local snapshot archives for all six targets and the broker
  image: dependency notices, root Go license, CSV inventories, Windows-only
  dependency attribution, and no eval/broker binary in CLI archives. Smoke-test
  the preview copy/archive recipe for all six targets without publishing.

`scripts/licenses.sh` generates the ignored `THIRD_PARTY_LICENSES` bundle with
per-target inventories, dependency license/NOTICE files, and the Go license and
patent grant. Stable archives, preview archives, and the broker's final Docker
layer include it; CI exercises all six release targets. The generator refuses
to replace unowned directories/symlinks and stages a complete result before
replacing a previous generated bundle. Contributor docs describe the process.
This is packaging work, not a formal legal compliance certification.

### 4. Existing-user migration guidance

- [x] Document targeted cleanup of old Roast-owned Claude PostToolUse settings,
  footer links, and `<claude-root>/hooks/roast/posttooluse-roast.sh`, preserving
  unrelated settings and hooks. New `skill install` does not remove them.
- [x] Document retired `gate.auto_install` / `ROAST_HOOK_INSTALL` behavior and
  old global `ROAST_API_KEY` / `AIGW_API_KEY` semantics versus explicit current
  profiles and role credential sources.
- [x] Rehearse the actual Homebrew upgrade, then use the installed binary for
  archive-style configuration-URL migration with dummy credentials and live
  finder/verifier inference against the operator's unchanged current config.
- [ ] Exercise the production configuration-URL/upload service with deliberate
  upload consent. This was not part of the authorized local installation test;
  real uploads remain disabled. Local model/broker/storage fixtures cover the
  automatic-upload protocol and successful outbox state below.

The public guide is `docs/upgrading.md`. No local Claude settings were inspected
or changed. The earlier audit wording was too definite: stale entries can
remain after upgrading; their presence on the operator's machine was not verified.

- [x] Exercise the real CLI upgrade in isolated tests with local model/broker/
  storage HTTP fixtures: both in-place and separate-directory provisioning,
  legacy backups, finder/verifier review, durable bundles, automatic upload,
  successful outbox state, and secret-free metadata.

This test uncovered a presigned-upload ownership bug: HTTP closed the caller's
bundle file, so Roast's later close failed and a successful upload stayed
pending. The uploader now gives HTTP a non-owning reader wrapper. File-reader
and end-to-end regression tests cover the fix. That isolated rehearsal did not
change the installed executable or live tap; the subsequent authorized stable
release and actual Homebrew upgrade are recorded below.

### 5. Stable-dependency follow-up

- [x] Assess whether upstream requirements permit replacing the selected
  `google.golang.org/grpc v1.85.0-dev.0.20260825072537-93e31b48545e` and
  `go.yaml.in/yaml/v4 v4.0.0-rc.6` with stable releases.

Checked October 2: grpc's latest stable 1.84.0 has GO-2026-6443, a server-panic
advisory whose fixed version is exactly the selected development pin. A trial
downgrade added a package-level advisory (still no reachable vulnerable call),
so it was reverted. Keep the patched pin until upstream ships a fixed stable.
YAML v4's latest release is still rc.6, and upstream JSON-schema/Anthropic SDK
code imports its v4 types. A v3 module substitution would not be a compatible
cleanup. Runtime module files are unchanged; these two explicit exceptions
are documented in CONTRIBUTING.md rather than left as unexplained upgrade TODOs.

## Intentionally deferred: separate eval binary

Confirmed October 2, 2026: eval, replay, adjudication, `runs label`, and
`internal/label` belong in a totally separate binary, not the main Roast CLI or
its Homebrew package. Their omission here is intentional, not an OSS-readiness
gap or release blocker. Do not pull them into this migration.

Separate-binary work is tracked in PLA-1037. Its detailed scope and tracking
will be handled with that work, not as a follow-up required for this release.

No Linear issue was modified during this audit.

## Homebrew checkpoint

- [x] Commit new-repo publishing setup: `2ec3f01`,
  `fix(release): take over Homebrew publishing`.
- [x] Commit archive retirement: `b7d8a17`,
  `chore(release): retire archive Homebrew publishing`.
- [x] Verify organization `TAP_GITHUB_TOKEN` access includes `roast` and excludes
  `roast-archive`. No secret value was retrieved or printed.
- [x] Verify both archive release workflows remain disabled in GitHub.
- [x] Keep the existing `bluesky-social/tap/roast` cask identity. New stable
  publishing writes directly to the existing tap, without another switch.
- [x] Check GoReleaser snapshots, six CLI targets, and cask metadata/checksums.
- [x] Check native public downloads without `gh`, using an already public
  third-party release asset. No Roast asset was made public for this test.
- [x] Check authenticated private downloads. Also verify anonymous denial for
  an actual existing `roast` preview-3-1 macOS arm64 asset: curl, with its local
  configuration disabled and no GitHub credential, returns HTTP 404.
- [x] Keep credential preflight in the stable release job, so normal release-PR
  maintenance does not require the tap token.
- [x] Push the prepared commits and audit fixes (`bf2c1e3`) to the new repo;
  push archive retirement (`b7d8a17`) to the archive. Integrate the previously
  approved remote Dependabot update with a normal merge, not a history rewrite.
- [x] Verify remote CI for prepared main (`37067091356`), release PR
  (`37067125540`), released main (`37067809440`), and archive retirement
  (`37066870902`). All pass.
- [x] Publish `v1.1.0` by merging Release Please PR #3. Release commit:
  `a7099a3`. Publisher run `37067809367` succeeds, attaching six CLI archives
  and checksums and updating the existing cask from the new repository. Tap
  commit: `6b0b3ff3b81a291ec76c1135d04d910829085f2e`.
- [x] Run the actual `brew update` / `brew upgrade --cask roast` locally.
  Homebrew upgrades its receipt from 1.0.14 to 1.1.0 and replaces the manually
  linked preview with `/opt/homebrew/Caskroom/roast/1.1.0/roast`.
- [x] Inspect the downloaded release archive: checksum matches the cask, Go
  license and six inventories are present, and no eval/broker binary ships.
- [x] Run installed-binary `doctor`, configuration-URL migration, and custom
  skill installation/status. Both real provider roundtrips pass; dummy legacy
  migration preserves its byte-identical backup, imports the intended inference
  credential, discards retired fields, leaves uploads disabled, and writes
  mode-600 config/backup files.
- [x] Review a seeded slice-guard regression using the installed binary.
  Finder and independent verifier each make one call; the defect is confirmed,
  findings exit code 1 is expected, and `runs show` reads the persisted report.
  Run: `20261002T215456Z-6dd887e5`.
- [x] Verify the operator's real config is byte-for-byte unchanged and uploads
  remain disabled. No Claude settings or existing global skills were changed.
- [x] Verify both repositories remain private. Anonymous access to the actual
  stable macOS arm64 asset returns HTTP 404; Homebrew's authenticated download
  succeeds. The installed `doctor` release-check warning is expected because
  that informational checker does not authenticate to the private repository.

Before this authorized October 2 release, the live tap was 1.0.15 from the
archive and the local receipt was 1.0.14 with preview-3-1 linked manually. The
live cask and local installation now both use 1.1.0 from the new repository.
This completes the private stable cutover/local-install test, not the public
visibility or final history-rewrite phase.

## Audit and validation journal

- [x] Compare command trees, module areas, prompt templates/tests, config
  migration, local integration, and release packaging across both checkouts.
- [x] Scan current source/docs/workflows for internal endpoints, environment
  assumptions, retired services, and leftover public-migration references.
  Public project ownership/import paths and security contact are expected.
- [x] `go test ./...` and `go vet ./...` pass in the new repository.
- [x] October 2 fixes also pass focused race tests, Staticcheck, actionlint,
  shellcheck, and `git diff --check`. Local GoReleaser snapshot and broker Docker
  builds succeed. The stopped image-inspection container and temporary preview
  packaging directories were removed. This validation initially ran without
  publishing; the later authorized remote release/install checks are above.
- [x] `govulncheck v1.8.0 ./...` reports zero reachable vulnerabilities. It also
  reports GO-2026-5932 for the unmaintained `golang.org/x/crypto/openpgp` package,
  which Roast neither imports nor calls. No fixed version is listed. This is
  not an assertion that every required module is vulnerability-free.
- [x] New-repo redacted Gitleaks scan: 28 reachable commits, no detections.
- [x] Triage archive history scan: 282 commits, 216 detections, all in three
  vendored GitLab client test/setup files. Every detected historical copy has
  one of the same three Git blob IDs; all three files match public upstream
  `gitlab-org/api/client-go` v1.9.1 byte-for-byte. These are upstream fixtures,
  not identified internal credentials. No credential values were printed or
  used for authentication, and the archive remains private.
- [x] Check the operator's configured provider roundtrips with `doctor`; both
  succeed, uploads are disabled, and the stable-release check warns as expected
  while no new stable release exists.
- [x] Run adversarial review on this reconstructed journal: five candidates
  refuted; one LOW download-validation ambiguity addressed with the explicit
  anonymous private-asset test and separate checklist entries above.
- [x] Run adversarial review on the October 2 integrated fixes
  (`20261002T211914Z-da383c1f`). The one retained candidate had an
  insufficient-context verdict for overlapping archive globs. Actual snapshot
  output showed deduplication, not corrupt archives; remove the redundant glob
  anyway. A fresh snapshot has no duplicate warnings/members and its notices
  match the complete generated bundle for all six targets. The five dropped
  candidates do not identify additional defects: the legacy schema covers the
  archive's full top-level configuration, and the generator's network/tool,
  container regeneration, cleanup, and union-copy behavior are intentional.

Tracked Go measurements excluding `vendor/`, including tests, at this
checkpoint: archive 191 files / 63,738 lines / 93 test files; new repo 81 files /
22,111 lines / 34 test files. This is a migration-scope comparison, not a
controlled before/after measurement of the Fantasy rewrite alone.

## Final publication phase, not authorized yet

- [x] Complete the approved upgrade fixes, tests, notice packaging, private
  stable cutover, and actual local Homebrew installation checks above. Human
  attribution review and the optional production upload rehearsal remain open.
- [ ] Final legal/content review for internal details, credentials, employee or
  user information, reputational commentary, and third-party material. Automated
  secret scans are not a substitute for this review.
- [ ] Review the final reachable history and GitHub content, including branches,
  tags, preview releases/assets, PRs, issues, and workflow logs/artifacts. A
  squash alone does not remove older refs or release artifacts.
- [ ] Remove or condense this temporary journal; keep public documentation in
  sync with the system being released.
- [ ] Perform the final squash/rebase only after explicit authorization, then
  rescan the exact intended public history and artifacts.
- [ ] Change repository visibility only after explicit authorization. Keep the
  archive private; do not treat opening the new repository as permission to
  open or rewrite the archive.

Neither final history rewriting nor a permission change has been performed.
