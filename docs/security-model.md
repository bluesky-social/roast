# Security model

Roast reviews source code by sending selected repository material to inference
services. This document describes the boundaries operators should understand;
it is not a promise that a model or provider will handle untrusted input safely.

## Trusted configuration

Inference profiles, credentials, upload destinations, and prompt overrides come
from the user's Roast configuration, environment, or command-line flags. Roast
does not load repository-local configuration. A checked-out repository therefore
cannot select a network destination or credential merely by adding a config
file.

## Direct ChatGPT plan usage

On macOS and Linux, run `roast chatgpt signin` and select a `chatgpt` profile.
Roast uses `https://auth.openai.com` for sign-in, refresh, and revocation
and sends access tokens to `https://api.openai.com/v1` for model listing
and inference. Leave `endpoint`, `api_key`, `api_key_env`, and `session_header`
unset. Roast rejects role overrides for the endpoint and API key and ignores
`OPENAI_BASE_URL` and `OPENAI_API_KEY` for this protocol.

Roast stores registrations, tokens, and pending refresh responses in the
system keyring through [go-keyring](https://github.com/zalando/go-keyring).
On macOS, access goes through `/usr/bin/security` and follows that tool's Keychain
permissions. On Linux, a Secret Service provider must be reachable on your
user D-Bus session, with a login or default collection that you can unlock.
For unattended runs, arrange for that collection to be unlocked or use an
API-key profile. A locked collection can prompt for unlock.

Roast uses a private lock file to serialize account updates and token renewal.
On macOS it lives in the system's per-user directory. On Linux it lives in the
user's home, which must be user-owned and have no shared write permissions.
Keep the lock location under directories you trust, and leave the lock file
in place while any Roast command is running.
Sign out before moving your user config directory so its old entries can be
revoked and removed from the keyring.
Each update saves new keyring entries before publishing the snapshot that
readers use. Roast cleans up entries left by an interruption before attempting
another token renewal.

When an unusable refresh reply contains a complete, distinct replacement
refresh token, Roast asks OpenAI to revoke it. It ignores credentials echoed
in OAuth errors. If revocation cannot be confirmed, disconnect Roast in ChatGPT
Settings. If OpenAI's identity verification keys are temporarily unavailable,
Roast saves the replacement and retries verification on the next request.

If the connection drops after OpenAI rotates a token but before Roast receives
the replacement, or Roast exits before saving it, you may need to sign in again.
To revoke that unseen replacement, disconnect Roast in ChatGPT Settings.
Signing out asks OpenAI to revoke the grant, then clears the local credentials
even if OpenAI cannot be reached. If a sign-out reported unconfirmed remote
revocation, check Connected Apps in ChatGPT Settings after retrying local cleanup.
To reconnect a saved account, fix any reported
storage problem, then run `roast chatgpt signin --account CLIENT_ID` with the ID from
`roast chatgpt status`. If you used an earlier file-backed preview, sign in
again to populate the keyring. First sign out using the preview, or disconnect
its registration in ChatGPT Settings. Then remove the preview's credential file.
For a file-backed preview, check its profile directory for `accounts.json`
and any `.credentials-*` files left by interrupted writes.

## Configuration templates

An organization can publish a configuration template at a URL. A template is
installed by `roast config init --from <url>`, by `roast config init` with the
URL typed at its prompt, or by entering the URL at the interactive
configuration-URL prompt that `roast review` and `roast runs sync` can show
(described below). Templates are trusted input: review and host them with the
same care as other organization configuration.

The configuration URL must use HTTPS, or plain HTTP to a loopback address, and
must not embed credentials or a fragment. Redirects must stay on the same
origin, and the document is limited to 1 MiB. Roast validates the complete
template before writing anything, installs it atomically with owner-only
(`0600`) permissions, and never sends a local credential to the configuration
URL. A template that only switches existing Claude profiles to the native
`anthropic` protocol changes nothing else in an existing configuration, and the
previous file is kept as a backup.

A template may set:

- inference profiles: model, endpoint, protocol, output-token limit, and the
  name of the environment variable that holds each profile's credential
  (`api_key_env`);
- role selection and author routes;
- review defaults;
- upload settings, including enabling automatic uploads, the destination, and
  the names of the environment variables that hold upload credentials
  (`access_key_env`, `secret_key_env`, `session_token_env`, `token_env`).

A template may not contain a literal `api_key` or upload `token`, and may not
set `prompts.*`: prompt overrides name local files whose contents would be sent
to the template's endpoints, so they belong only in the user's own config.

Because a template can name any environment variable as a credential source and
can direct reviewed code and uploads to any endpoint, Roast prints a summary to
stderr whenever it installs one: for each role (and author route), the profile,
endpoint, protocol, and credential variable, plus whether uploads are on and
where they go. When a template enables uploads during an interactive install,
Roast installs it only after an explicit `y` answer to a `[y/N]` question.
Non-interactive installs print the summary and proceed, since the command
itself names the URL.

A template's text fields, such as profile names, author-route keys, endpoints,
and the upload bucket and prefix, must not contain control characters; Roast
rejects such a template before writing anything. The summary and the upload
question also escape control characters in every configuration value they
print, so configuration text cannot rewrite what the user is asked to approve.

The configuration-URL prompt appears only when both stdin and stderr are
terminals and no `--config` path is given. It then appears in three cases: the
default config file is missing and the finder and verifier cannot be resolved
from the environment and command-line flags; the file is unversioned; or the
file has Claude profiles using Chat Completions or Responses. The last two cases
prompt even when the roles already resolve. Leaving the URL blank leaves the
configuration as it is; for a versioned file, that choice is recorded in a
private sibling `.protocol-update-ack` file containing only the config's hash,
so the prompt does not repeat until the config changes. Its prompts go to
stderr, so redirected review output is never mixed with them.

## Credentials and endpoints

API-key profiles bind a model, endpoint, protocol, and credential source.
Provider environment variables supply fallback values.

- `ROAST_FINDER_*` and `ROAST_VERIFIER_*` variables override the selected
  profile's fields, and command-line role flags override both.
- `OPENAI_BASE_URL` (for `chat` and `responses`) or `ANTHROPIC_BASE_URL` (for
  `anthropic`) applies only to a profile that sets no `endpoint`, including
  after a protocol override moves that profile to another provider.
- A role's credential is the first of: `ROAST_FINDER_API_KEY` or
  `ROAST_VERIFIER_API_KEY`; the profile's `api_key_env` or `api_key`; or the
  provider's `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`, but only when the role's
  endpoint is that provider's default endpoint or the same provider's
  `*_BASE_URL`.
- When a protocol override moves a role from one provider's endpoint to
  another's, the profile's credential does not follow it.
- An endpoint override (`ROAST_<ROLE>_BASE_URL` or `--<role>-base-url`)
  carries the role key or the profile's own credential to the endpoint it
  names. It carries a provider key from the shell only under the rule above:
  when the endpoint it names is that provider's default endpoint or the same
  provider's `*_BASE_URL`.
- `ROAST_<ROLE>_*` variables and the matching command-line flags apply these
  rules identically.

A provider key in the user's shell is therefore never sent implicitly to a
profile's custom endpoint, such as a gateway or a template's endpoint. A
profile with such an endpoint must name its credential, and the name is not
restricted: a profile, including one installed from a template, may set
`endpoint: https://models.example.org/v1` with `api_key_env: OPENAI_API_KEY`,
and that key is then sent to that endpoint. The install summary lists the
credential variable of each profile that a role or author route selects, and
the upload credential variables, so review it before relying on a template.
`roast doctor` reports which source each role used, and names a provider key it
withheld.

## Network disclosure

A review sends its prompt, selected diff or repository manifest, file content,
tool schemas, and local tool results to the configured finder endpoint. Candidate
findings and the code needed to adjudicate them are sent to the configured
verifier endpoint. For each valid role, `roast doctor` runs a two-turn probe
with a fixed prompt, a diagnostic tool call and result, and a final submission.
It also queries the model list for API-key profiles.

Each `chatgpt` inference request sends the conversation so far, including
prior replies, completed reasoning, and tool results. Roast sets `store: false`
to ask OpenAI to skip saving the response. It validates the completed response
before running local tools. Reviews and the `roast doctor` probe count toward
your plan. The output-token budget is a local estimate.

API-key inference requests use only the credential, base URL, and headers from
Roast's resolved configuration. Roast ignores the provider SDKs' ambient
defaults, such as `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, Anthropic
profile files, `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`, and
`OPENAI_CUSTOM_HEADERS`; `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and the
`*_BASE_URL` variables apply only as inputs to Roast's own configuration.
An API-key profile's `session_header`, which a template may also set, adds a
header carrying the per-conversation session ID. Roast rejects names reserved
for credentials or framing and names set by Roast or the provider SDK.

Roast does not infer trust from a model name. An OpenAI-compatible gateway,
hosted provider, and local server are all treated as the operator-configured
destination. Operators are responsible for checking those services' retention,
training, and access-control policies before reviewing sensitive repositories.

Run uploads are disabled by default. When enabled, Roast sends a deterministic
bundle containing the complete local run record to the configured S3-compatible
endpoint or presigned-upload broker. Upload endpoints, and the presigned URLs a
broker returns, must use HTTPS unless they are on loopback or
`upload.allow_insecure_http` is set. Failed uploads record the destination
without a presigned URL's signed query string. The bundled broker expects
authentication to be enforced by a trusted reverse proxy; it must not be
exposed directly to untrusted clients.

Builds that report a version other than `dev` check GitHub for a newer stable
release by sending an unauthenticated `GET` to the repository's latest-release
API endpoint, with Roast's version in the `User-Agent` header. These include
release binaries, `go install` builds, and source builds from a Git checkout,
which the Go toolchain stamps with a version. The check sends no review data,
repository content, or credentials. Commands reuse a cached result for up to a
day, `roast doctor` checks on each run, builds that report `dev` skip the check,
and `ROAST_NO_UPDATE_CHECK=1` disables it.

## Local sensitive data

Every non-empty review records the reviewed input, prompts, raw successful
provider responses, findings, report, metadata, and a Git snapshot used for
reproduction. These artifacts may contain proprietary source code, secrets
already present in reviewed content, or sensitive model output. Protect the
Roast data directory and repository's `refs/roast/snapshots/*` with the same
controls as the repository itself, and prune them according to local policy.

On Unix, Roast creates managed run directories with owner-only (`0700`)
permissions and artifacts, monthly records, and bundles with `0600` permissions.
Existing managed directories and files are tightened when written or reused.
New bundle entries also use `0600`; cached bundles retain their original bytes
and digest, so older archive entries may still carry their original permissions.
Roast does not change permissions on an existing configured data root or its
ancestors, and Unix permission bits do not establish Windows ACL isolation.

Known credential environment values are excluded from run metadata. Roast
cannot guarantee that a repository, model, provider, or tool result will not
echo a secret into an artifact or response.

## Untrusted repository and model input

Repository paths supplied by a model are confined beneath the reviewed tree;
absolute paths, traversal, unsafe Git revision spellings, escaping symlinks,
and paths inside `.git` are rejected. Every review reads a pinned Git tree, even
when no replayable snapshot can be recorded, so concurrent checkout changes
cannot alter the evidence seen during a run. If Git cannot write that tree,
Roast reads the live working tree instead but refuses ignored files and
untracked files matching its secret denylist.

Model output is schema-validated. Findings without a source quote grounded in
the reviewed corpus are dropped, and a separately configured verifier checks
surviving claims. These checks reduce accidental fabrication; they do not make
model output trusted executable input.

## Git hooks

On the first interactive review in a repository, Roast asks whether to install
its pre-commit gate. The answer is stored in that repository's local Git config,
and a decline is not prompted again. Non-interactive runs never install a hook
without consent. Roast does not overwrite foreign hooks: compatible shell hooks
receive a delimited invocation block, while incompatible hooks require a manual
step.

Gate dispatch requires this repository's local `roast.gate` opt-in, even when
`core.hooksPath` points to a shared directory. An inherited dispatcher is not
consent. Disabling a repository with a custom hooks path records a local decline
and preserves the dispatcher and foreign hooks for other repositories. Existing
Roast-owned dispatchers are refreshed to enforce this check when encountered;
foreign scripts are not rewritten.

The gate reviews staged content and fails open on operational errors. Use
`roast gate status`, `roast gate enable`, and `roast gate disable` to inspect or
change enrollment. `ROAST_SKIP=1` bypasses one commit.

## Vulnerability reports

See [the security policy](../SECURITY.md) for supported versions and private
reporting instructions.
