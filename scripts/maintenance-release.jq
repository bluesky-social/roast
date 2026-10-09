# Only Release Please's stable patch PRs are unattended releases. GitHub owns
# merge/check enforcement; this filter does not replace branch protection.
def stable_version:
  capture("^(?<major>0|[1-9][0-9]*)\\.(?<minor>0|[1-9][0-9]*)\\.(?<patch>0|[1-9][0-9]*)$")
  // error("invalid stable version")
  | map_values(tonumber);

try (
  ($base_version | stable_version) as $before
  | ($next_version | stable_version) as $after
  | .author.is_bot == true
    and (.author.login == "app/github-actions" or .author.login == "github-actions[bot]")
    and .baseRefName == "main"
    and .headRefName == $release_branch
    and .isCrossRepository == false
    and .title == ("chore(main): release " + $next_version)
    and (.files | map(.path) | sort) == [".release-please-manifest.json", "CHANGELOG.md"]
    and ([.labels[].name] | index("release:manual") | not)
    and $after.major == $before.major
    and $after.minor == $before.minor
    and $after.patch > $before.patch
) catch false
