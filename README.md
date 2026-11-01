# Roast

Roast is an adversarial cross-model code reviewer. A finder proposes potential
issues, then an independent verifier checks each claim against the code before
it reaches the final report.

Roast reviews a Git diff, requires every candidate to quote the code and offer
a concrete fix, then asks a second model to confirm or refute the claim. The
finder and verifier may use different endpoints and wire protocols.

## Current status

Roast supports local review, repository survey, durable run records,
an optional Git gate, and optional S3-compatible run uploads. It has no
telemetry or external retrieval tools. In interactive mode, models can only
navigate the local checkout with bounded file, grep, and listing tools.

## Build

Roast requires Go 1.27.3 or newer:

```sh
go build ./cmd/roast
```

## Install

For macOS with Homebrew:

```sh
brew install --cask bluesky-social/tap/roast
```

Existing installations from `bluesky-social/tap` upgrade in place with
`brew upgrade --cask roast`; no re-tap or package rename is needed.
See [Upgrading from the original Roast](docs/upgrading.md) for configuration
migration and optional cleanup of retired Claude integrations.

Tagged releases provide `roast` archives for macOS, Linux, and Windows on
amd64 and arm64. Download the archive for your platform from
[GitHub Releases](https://github.com/bluesky-social/roast/releases) and verify
it against the published SHA-256 checksum file.

With Go 1.27.3 or newer, you can instead install the latest tagged version:

```sh
go install github.com/bluesky-social/roast/cmd/roast@latest
```

Source builds use the command shown above. Run `roast --version` to inspect the
version, commit, and build date stamped into a release binary.

Released binaries perform a cached, informational check for newer stable GitHub
releases. The check never blocks a command or changes its exit status. Network
and cache failures fail open; update with Homebrew or the installation method
you originally used. Set `ROAST_NO_UPDATE_CHECK=1` to disable the check.

To install the bundled Agent Skill:

```sh
roast skill install
roast skill status
```

Roast always installs to the shared `~/.agents/skills/roast` location and also
installs to `$CLAUDE_CONFIG_DIR/skills/roast` (or `~/.claude/skills/roast`) when
Claude Code is detected, and to `$XDG_CONFIG_HOME/polytoken/skills/roast` (or
`~/.config/polytoken/skills/roast`) when Polytoken is detected. `roast skill
status` reports every detected target. Use `--dir` to operate on only one
explicit parent skills directory.

## Configure

Both roles require a model name and API key. By default they use the OpenAI
Responses protocol at `https://api.openai.com/v1`:

```sh
export OPENAI_API_KEY='...'
export ROAST_FINDER_MODEL='your-finder-model'
export ROAST_VERIFIER_MODEL='your-verifier-model'
```

Each role can instead use its own endpoint and protocol:

```sh
export ROAST_FINDER_BASE_URL='https://finder.example/v1'
export ROAST_FINDER_API_KEY='...'
export ROAST_FINDER_MODEL='finder-model'
export ROAST_FINDER_PROTOCOL='chat'

export ROAST_VERIFIER_BASE_URL='https://verifier.example/v1'
export ROAST_VERIFIER_API_KEY='...'
export ROAST_VERIFIER_MODEL='verifier-model'
export ROAST_VERIFIER_PROTOCOL='responses'
```

Native Anthropic Messages is also supported. It uses `x-api-key`
authentication and defaults to `https://api.anthropic.com/v1` when
`protocol: anthropic` is selected:

```sh
export ANTHROPIC_API_KEY='...'
export ROAST_FINDER_MODEL='claude-model-name'
export ROAST_FINDER_PROTOCOL='anthropic'
```

The supported protocol values are `chat`, `responses`, and `anthropic`. Custom
base URLs work for each protocol, so a role can use a hosted provider, gateway,
or local server that implements the selected wire contract.

Configuration may also be stored in the user config directory at
`roast/config.yaml`, or supplied with `--config`:

An organization may publish a secret-free configuration template. Install it
once with `roast config init --from https://config.example/roast`; an
interactive first run also offers this prompt when no current config exists.
When upgrading an unversioned pre-public config, Roast carries its existing
credential into otherwise credential-free inference profiles and presigned
upload configuration, leaves a `.pre-v1` backup for an in-place migration,
and never sends that credential to the configuration URL.

Credential import is disabled unless the trusted template explicitly contains:

```yaml
provisioning:
  import_legacy_credential: true
```

```yaml
version: 1

inference_profiles:
  claude:
    endpoint: https://api.anthropic.com/v1
    model: claude-model-name
    protocol: anthropic
    api_key_env: ANTHROPIC_API_KEY
    max_output_tokens: 32768
  verifier:
    endpoint: https://verifier.example/v1
    model: verifier-model
    protocol: responses
    api_key_env: OPENAI_API_KEY
    max_output_tokens: 32768

roles:
  finder:
    profile: claude
  verifier:
    profile: verifier

# Optional: send each completed deterministic bundle directly to an
# S3-compatible object store. Disabled unless explicitly enabled.
upload:
  enabled: true
  backend: s3
  endpoint: https://objects.example.com
  bucket: roast-runs
  prefix: team-name
  region: us-east-1
  access_key_env: ROAST_UPLOAD_ACCESS_KEY
  secret_key_env: ROAST_UPLOAD_SECRET_KEY
  # session_token_env: ROAST_UPLOAD_SESSION_TOKEN
  path_style: false
  # allow_insecure_http: false
```

Organizations which do not distribute object-store credentials can instead
use a presigned upload broker:

```yaml
upload:
  enabled: true
  backend: presigned
  endpoint: https://roast-uploads.example.com
  token_env: ROAST_UPLOAD_TOKEN
```

`version: 1` identifies the public configuration contract. Roast also accepts
two legacy unversioned formats and migrates them in memory: inline
role settings using `api`/`author_flip`, and named profiles using `base_url`.
An explicit unsupported version or an unknown field is an error.
The old inline-role format recognizes and discards its retired `gate` and
`sourcebot` sections; they are not valid fields in version-1 configuration.

Each profile names a model and the HTTP `endpoint` that serves it. `protocol`
selects the Fantasy wire adapter (`chat`, `responses`, or `anthropic`). The
provider is simply the service behind that endpoint; Roast does not infer a
wire protocol or credential boundary from the model name.

`api_key_env` names an environment variable; its value is resolved only when
the profile is selected. A literal `api_key` is also supported in the
user-owned config, but environment references are preferable. Role-specific
`ROAST_FINDER_*` and `ROAST_VERIFIER_*` variables override profiles, and
command-line role flags override both. Roast does not read repository-local
configuration, so a checkout cannot redirect inference or supply credentials.

Provider variables are a fallback, never an override. `OPENAI_API_KEY` and
`OPENAI_BASE_URL` apply only to `chat` and `responses`; `ANTHROPIC_API_KEY` and
`ANTHROPIC_BASE_URL` apply only to `anthropic`. A provider `*_BASE_URL` applies
only to a profile that sets no `endpoint`. A role's credential is the first of:

1. `ROAST_FINDER_API_KEY` or `ROAST_VERIFIER_API_KEY`;
2. the profile's `api_key_env` or `api_key`;
3. the provider `*_API_KEY`, only when the role's endpoint is that provider's
   default endpoint or the same provider's `*_BASE_URL`.

A profile with any other endpoint must name its own credential, so a provider
key from your shell is never sent to a gateway or a template's endpoint. When
a protocol override moves a role from one provider's default endpoint to
another's, the profile's credential does not follow it. `roast doctor` reports
which source each role used.

For the `s3` backend, upload credentials are resolved only from the environment variables named by
`access_key_env`, `secret_key_env`, and optional `session_token_env`. Roast
does not consult AWS profiles, SSO, instance metadata, or another provider
credential chain. `endpoint`, `region`, and `path_style` support services such
as MinIO, Cloudflare R2, and other S3-compatible object stores. Upload
endpoints must use HTTPS unless they are on loopback. Plaintext HTTP on another
trusted local network requires `allow_insecure_http: true`; it sends bundle
contents and SigV4 authentication material without transport encryption and
must never be used across an untrusted network.

The `presigned` backend sends the configured bearer token only to the broker's
control-plane endpoint. The broker returns a short-lived PUT URL and required
signed headers; Roast sends the bundle directly to object storage without the
bearer token, then asks the broker to verify completion. The broker owns the
bucket name, object prefix, and storage credentials.

When the invoking agent supplies `--author-model` or `ROAST_AUTHOR_MODEL`, the
finder can select another complete profile by exact model name or model family:

```yaml
version: 1

inference_profiles:
  primary:
    endpoint: https://api.openai.com/v1
    model: gpt-reviewer
    protocol: responses
    api_key_env: OPENAI_API_KEY
  independent:
    endpoint: https://api.anthropic.com/v1
    model: claude-reviewer
    protocol: anthropic
    api_key_env: ANTHROPIC_API_KEY

roles:
  finder:
    profile: primary
    author_routes:
      openai: independent
      exact-author-model: independent
```

The selected profile contributes its model, endpoint, protocol, output-token
limit, and credential source as one unit. Roast records the profile name and
resolved non-secret fields, but excludes API keys and captured `*_API_KEY`
environment values from run artifacts. The invocation argv is recorded, so do
not place secrets in command-line values. Prompt overrides are user-owned files:

```yaml
prompts:
  finder_code: /absolute/path/to/finder.txt
  finder_prose: /absolute/path/to/prose-finder.txt
  verifier: /absolute/path/to/verifier.txt
```

## Use

Run Roast inside a Git repository:

```sh
roast review --task 'add retry handling'
roast review --staged --max-severity HIGH
roast review --interactive
roast review --quick
roast review --scope repo
roast runs
roast runs show <run-id>
roast runs status
roast runs sync
roast doctor
```

`roast doctor` validates the resolved profiles and credentials, performs an
authenticated `GET` of each provider's `/models` endpoint, then makes one small
tool-submission inference request through each configured model and protocol. It
also reports Git/gate, Agent Skill, upload, and release status. Use `--json` for
machine-readable results.

Finalization uses automatic tool selection with only the result tool available.
Roast accepts only a schema-validated tool submission, not free-form text. Missing
submissions receive bounded reminders within the existing attempt, token, and
deadline limits; an exhausted run is incomplete, never silently clean.

On the first interactive review in a repository, Roast asks once whether to
install its staged-content pre-commit gate. The answer is stored in local Git
config for that repository; a decline is not prompted again. Non-interactive
runs never install a hook. Manage the decision explicitly with
`roast gate enable|status|disable`.

Shared `core.hooksPath` dispatchers still require an opt-in in each repository's
local Git config. Disabling one repository preserves a custom hooks directory's
dispatcher for other repositories.

The default scope includes committed changes since the best available main
branch plus uncommitted and untracked changes. JSON is written to stdout and
Markdown to stderr; select one with `--format`.

Source code, diffs, prompts, tool schemas, and local tool results are sent to
the inference endpoints you configure. Chat and Responses use bearer
authorization; native Anthropic uses `x-api-key`. When upload is enabled, the
complete run bundle is also sent to the configured object-store endpoint.
Roast sends no review data anywhere else. Review your providers' data-handling
terms before using Roast on sensitive repositories. See the
[security model](docs/security-model.md) for the complete trust and artifact
boundaries.

Every inference request carries an `X-Agent-Session-Id` identifying its Roast
conversation. Tool turns, structured-output corrections, and retries retain the
same ID. Separate finder calls and each independent verifier conversation get
different IDs, including when they share a client or run concurrently. The ID
does not identify a person and is not a per-request ID or a whole-review run ID.

## Resource limits

Roast applies the same resource controls to hosted providers, gateways, and
local inference servers. Defaults are intentionally generous enough for slow
local models, and every duration or run-level ceiling that may need tuning is
operator-controlled:

| Control | Default | Override |
| --- | ---: | --- |
| Output tokens per model call | 32,768 | Profile `max_output_tokens` or role flag |
| Timeout per inference request | 10 minutes | `--request-timeout` / `ROAST_REQUEST_TIMEOUT`; `0` disables |
| Interactive finder work deadline | 10 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Repository-survey work deadline | 25 minutes | `--deadline` / `ROAST_DEADLINE`; `0` disables |
| Provider/structured-output attempts | 4 | `--max-attempts` / `ROAST_MAX_ATTEMPTS` |
| Shared finder/verifier token budget | 3,000,000 | `--max-tokens-total` / `ROAST_MAX_TOTAL_TOKENS`; `0` disables |
| Findings sent to the verifier | 25 | `--max-verified` / `ROAST_MAX_VERIFIED`; `0` disables |
| Diff bytes in a normal/quick prompt | 1,000,000 / 200,000 | `ROAST_MAX_DIFF_BYTES` |
| File-context bytes in a normal/quick prompt | 400,000 / 100,000 | `ROAST_MAX_FILE_CTX_BYTES` |
| Initial finder prompt bytes | 8,000,000 | `ROAST_MAX_PROMPT_BYTES` |

Interactive finder runs default to 16 turns; repository surveys default to 40.
`--max-turns` changes either value. Once an agent conversation grows beyond
roughly 3 MB, Roast tells it to stop exploring and submit its accumulated
result. A finder work deadline gets a separate 90-second final-submission
window so the deadline does not discard work already found.

Some defensive caps are intentionally not configurable: one captured provider
response may be at most 16 MiB; one `git_diff` tool window is at most 64 KiB;
`read_file` truncates pathological individual lines at 1 MB; and `list_files`
and `grep_repo` return at most 500 paths and 200 matching lines respectively.
These caps protect memory, prompts, and run artifacts rather than expressing a
provider's context-window limit.

Invalid or negative resource overrides are usage errors instead of silently
falling back to another value. Resolved limits are recorded in run metadata.

## Exit and error contract

Roast preserves this process exit-code contract:

| Code | Meaning | Machine category |
| ---: | --- | --- |
| 0 | Clean completion | `none` |
| 1 | A finding met the requested `--max-severity` gate | `findings` |
| 2 | Invalid invocation or configuration | `usage` |
| 3 | Inference authentication failed | `auth` |
| 4 | Operational failure or incomplete result | `endpoint`, `resource_limit`, or `partial` |
| 5 | Caller cancellation or termination | `cancelled` |

Use `--error-format json` for stable machine-readable diagnostics on stderr:

```json
{"error_category":"endpoint","message":"...","exit_code":4}
```

This setting does not change successful review output: `--format json` still
writes the result to stdout, while diagnostics remain on stderr.

Every non-empty review creates a local run under `$XDG_DATA_HOME/roast`, or
`~/.local/share/roast` when that variable is unset (override either with
`ROAST_RECORD_DIR`). It records secret-free role configuration, invocation
argv, exact prompts and reviewed input, raw provider response bodies,
normalized usage, results, a Git snapshot, and a deterministic
`bundle.tar.zst`. Provider requests, HTTP headers, and credentials are not
recorded. The snapshot and digests make a run reproducible by external tooling
without recording API keys or other credential values.

Inference uses each configured protocol's native streaming implementation in
Fantasy. `provider-responses.jsonl` identifies each captured body's
`content_type`: JSON bodies remain JSON values, while `text/event-stream`
bodies are stored as JSON strings containing the original SSE bytes, including
partial streams from failed attempts. No inference protocol conversion is used.

Repository-navigation tools read that immutable snapshot during the run. For
staged reviews they read the captured index; other reviews read the captured
worktree, including captured non-ignored untracked files. Editing the checkout
after inference starts does not change finder or verifier context.

Each archive is a `roast-run-bundle` v1 bundle. Its generated
`bundle-manifest.json` inventories every archived artifact with its byte size
and SHA-256 digest. Roast verifies both new and reused bundles before accepting
them and refuses symlinked or other non-regular run artifacts.

When `upload.enabled` is true, Roast makes a best-effort upload after each
completed review. Direct-S3 objects use the content-addressed key
`<prefix>/sha256/<bundle-digest>.tar.zst`; the broker assigns a unique key and
binds its grant to the bundle's size and SHA-256. Upload failures never change the review result and remain pending
for `roast runs sync`; `roast runs status` reports local upload state. Runs
which predate the first opt-in are not uploaded unless sync is given
`--include-existing`. Invoking `runs sync` is itself an explicit upload request
and works even while automatic upload is disabled. Upload bookkeeping is
stored outside run directories and is never included in a bundle.

## Presigned upload broker

This repository includes the stateless `roast-broker` service and a root
`Dockerfile`. It expects authentication to be enforced by a trusted reverse
proxy. For `/api/v1/roast/runs` and its completion endpoint, the proxy must
remove the configured principal header from incoming requests, validate
`Authorization: Bearer`, and inject the verified principal. `/healthz` and the
optional, secret-free `/config/v1` are intentionally unauthenticated so a new
client can fetch configuration before it has credentials. Never expose the
container directly to untrusted clients, because all routes share one listener
and the API trusts only the proxy-injected principal header.

Required environment variables are:

```text
ROAST_BROKER_PRINCIPAL_HEADER=X-Forwarded-Email
ROAST_BROKER_SIGNING_KEY=<at least 32 random bytes>
ROAST_BROKER_S3_ENDPOINT=https://objects.example.com
ROAST_BROKER_S3_BUCKET=roast-runs
```

Static S3-compatible credentials may be supplied with
`ROAST_BROKER_S3_ACCESS_KEY`, `ROAST_BROKER_S3_SECRET_KEY`, and optional
`ROAST_BROKER_S3_SESSION_TOKEN`. With those unset, the MinIO client uses its
IAM/workload-identity credential source. Optional settings include
`ROAST_BROKER_S3_REGION`, `ROAST_BROKER_S3_PREFIX`,
`ROAST_BROKER_S3_PATH_STYLE`, `ROAST_BROKER_MAX_BUNDLE_BYTES`, and
`ROAST_BROKER_LISTEN_ADDRESS`.

Mounting a validated, secret-free client config and setting
`ROAST_BROKER_CLIENT_CONFIG_FILE` serves it at `/config/v1`, making the broker
itself a configuration URL. Startup fails if that document is unversioned,
invalid, or contains inline inference or broker credentials.

The version-1 broker protocol is deliberately small:

- `POST /api/v1/roast/runs` declares the run ID, bundle length, and SHA-256 and
  returns a content-bound presigned PUT plus an opaque completion token.
- The client performs the PUT with exactly the returned headers and without
  its broker credential.
- `POST /api/v1/roast/runs/{run_id}/complete` presents the opaque token; the
  broker verifies principal ownership and the stored object's length and
  checksum.

Each grant uses a unique object key, so the broker requires no database.
Operators should configure bucket lifecycle expiration for abandoned uploads.

Local run artifacts are retained indefinitely and are never deleted
automatically. `roast runs prune` only removes snapshot refs in the current
repository, defaults to refs older than 30 days, and supports `--dry-run`; it
does not delete run directories or bundles.

The optional local commit gate is explicit:

```sh
roast gate enable
roast gate status
roast gate disable
```

It reviews staged content and fails open on operational errors. Set
`ROAST_SKIP=1` to bypass one commit.

## Releases and commit messages

Releases are cut by [release-please](https://github.com/googleapis/release-please)
through the org release pipeline ([onboarding guide](https://github.com/bluesky-social/.github/blob/main/docs/reference/release-please-onboarding.md)).

**Commit messages need a `type:` prefix, but any word works.** PRs are
squash-merged, so the PR title becomes the commit, and a required `pr-title`
check enforces the prefix. release-please drops commits without one. `feat:`
releases a minor version, a breaking change (`feat!:`, or a `BREAKING CHANGE:`
footer) a major one (a minor while the version is 0.x), and anything else,
including subsystem prefixes like `ask:`, a patch. Release notes are built from
merged PR titles, so write the title as the changelog line you want. Direct
pushes appear only in the release's compare link.

**How a release happens.** Every push to `main` updates a standing
`chore(main): release X.Y.Z` PR maintained by `bsky-release-bot`. Merging it
tags `vX.Y.Z` and builds the release binaries and Homebrew cask (goreleaser).

- If every commit since the last tag came from a merged Renovate or
  Dependabot PR, the Release PR merges itself on the hourly run, weekdays
  09:00-16:00 PT. Otherwise a human merges it.
- Any human-authored commit in that range, even one that doesn't release on
  its own, means a human merges the next Release PR.
- Label the Release PR `release:manual` to hold a release.

**Dependencies** come from Renovate with the [org preset](https://github.com/bluesky-social/.github/blob/main/renovate/default.json)
(`renovate.json`). Don't push to a Renovate PR or click "Update branch" on
one: that adds a human commit, so the next release needs a human. If a bump
needs code changes, make them in a separate PR.

**CI.** `main` requires `Test and build`, `Security and licenses` and
`pr-title / pr-title` to pass, and PRs must be up to date with `main` before
they merge.

## License

This project is dual-licensed under MIT and Apache 2.0:

- [MIT License](LICENSE-MIT.txt)
- [Apache License, Version 2.0](LICENSE-APACHE.txt)

Downstream projects and end users may choose either license individually, or
both together, at their discretion.

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and contribution checks,
and [SECURITY.md](SECURITY.md) for the supported-version and private-reporting
policy. Runtime dependencies retain their own licenses; CI checks the complete
linked dependency graph against the project's allowed license set.
