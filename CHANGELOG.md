# Changelog

## 1.4.0 (2026-11-06)

## What's Changed
* docs: tighten the README for the public release by @amckinley in https://github.com/bluesky-social/roast/pull/122
* docs: explain tap trust for Homebrew upgrades by @amckinley in https://github.com/bluesky-social/roast/pull/124
* fix(deps): update module github.com/klauspost/cpuid/v2 to v2.4.2 by @renovate[bot] in https://github.com/bluesky-social/roast/pull/128
* fix: honor XDG_CONFIG_HOME on macOS by @blackmichael in https://github.com/bluesky-social/roast/pull/130
* docs: AUR package by @ThatOneCalculator in https://github.com/bluesky-social/roast/pull/126
* feat: per-profile session_header for gateways that key sessions on their own header by @amckinley in https://github.com/bluesky-social/roast/pull/131
* fix: keep replayed conversations byte-stable and name why a tool loop ended by @amckinley in https://github.com/bluesky-social/roast/pull/132
* fix: send real required fields to Anthropic and bump Fantasy to v0.46.0 by @amckinley in https://github.com/bluesky-social/roast/pull/133

## New Contributors
* @blackmichael made their first contribution in https://github.com/bluesky-social/roast/pull/130
* @ThatOneCalculator made their first contribution in https://github.com/bluesky-social/roast/pull/126

**Full Changelog**: https://github.com/bluesky-social/roast/compare/v1.3.0...v1.4.0

## 1.3.0 (2026-11-05)

First public release of Roast. The public repository starts from a single commit; releases v1.1.0 through v1.2.1 remain available on [GitHub Releases](https://github.com/bluesky-social/roast/releases).

### Bug Fixes

* The untracked-file secret denylist matches SSH private keys with suffixes (for example `id_ed25519_github`).
* Configuration templates containing control characters are rejected, and the install summary and upload confirmation escape template-provided values.
* Dependency updates.

## 1.2.1 and earlier

Earlier releases: see [GitHub Releases](https://github.com/bluesky-social/roast/releases).
