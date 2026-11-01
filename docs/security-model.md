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

Configuration templates installed with `roast config init --from` are trusted
input. Review and host those templates with the same care as other organization
configuration. The URL must use HTTPS, and Roast validates the complete result
before replacing the user's configuration.

## Network disclosure

A review sends its prompt, selected diff or repository manifest, file content,
tool schemas, and local tool results to the configured finder endpoint. Candidate
findings and the code needed to adjudicate them are sent to the configured
verifier endpoint. `roast doctor` makes a small tool-submission inference request to
each configured role in addition to querying its model-listing endpoint.

Each inference request carries only the credential, base URL, and headers from
Roast's resolved configuration. Roast ignores the provider SDKs' ambient
defaults, such as `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS`, Anthropic
profile files, `OPENAI_ORG_ID`, `OPENAI_PROJECT_ID`, and
`OPENAI_CUSTOM_HEADERS`; `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and the
`*_BASE_URL` variables apply only as inputs to Roast's own configuration.

Roast does not infer trust from a model name. An OpenAI-compatible gateway,
hosted provider, and local server are all treated as the operator-configured
destination. Operators are responsible for checking those services' retention,
training, and access-control policies before reviewing sensitive repositories.

Run uploads are disabled by default. When enabled, Roast sends a deterministic
bundle containing the complete local run record to the configured S3-compatible
endpoint or presigned-upload broker. The bundled broker expects authentication
to be enforced by a trusted reverse proxy; it must not be exposed directly to
untrusted clients.

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
absolute paths, traversal, unsafe Git revision spellings, and escaping symlinks
are rejected. Staged reviews and captured worktree reviews use a pinned Git tree
so concurrent checkout changes cannot alter the evidence seen during a run.

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
