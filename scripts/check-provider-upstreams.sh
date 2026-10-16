#!/usr/bin/env bash
#
# Compare every pinned guest provider package against the version npm currently
# publishes, and write the short Simplified Chinese summary a scheduled workflow
# pushes to the development DingTalk group and copies into the job summary.
#
# This exists so discovering an upstream release stops being a human chore. It
# only reports: it never edits a pin and never merges anything, because deciding
# whether a new provider version is safe is the part that needs a person and the
# guest image's acceptance tests.
#
# Outputs, under PROVIDER_UPSTREAM_OUTPUT_DIR (default: the current directory):
#   provider-upstream-notify.md     the summary pushed to DingTalk
#   provider-upstream-bump.txt      "kind=version" arguments for provider-lock.sh bump
#   provider-upstream-outdated      "true" or "false"
#
# The summary is Simplified Chinese because it is read in a DingTalk group by the
# development team. It lists only the packages that need attention: which pinned
# providers have a newer release upstream, which are retired by another provider's
# release, and how many npm queries failed. A package that is up to date, or that
# is pinned ahead of npm, is deliberately not listed.
#
# When GITHUB_OUTPUT is set, "outdated=<bool>" is also appended to it.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
LOCK_FILE=${PROVIDER_LOCK_FILE:-$ROOT_DIR/guest-images/providers.lock.json}
OUTPUT_DIR=${PROVIDER_UPSTREAM_OUTPUT_DIR:-$PWD}
MENTIONS=${PROVIDER_UPGRADE_MENTIONS:-}

NOTIFY_FILE="$OUTPUT_DIR/provider-upstream-notify.md"
BUMP_FILE="$OUTPUT_DIR/provider-upstream-bump.txt"
STATUS_FILE="$OUTPUT_DIR/provider-upstream-outdated"

[[ -f $LOCK_FILE ]] || {
  echo "check-provider-upstreams: lock file not found: $LOCK_FILE" >&2
  exit 1
}
jq -e . "$LOCK_FILE" >/dev/null 2>&1 || {
  echo "check-provider-upstreams: lock file is not valid JSON: $LOCK_FILE" >&2
  exit 1
}

mkdir -p "$OUTPUT_DIR"

# npm_view reads the version npm publishes for a package. A query failure is
# reported as "unknown" rather than aborting the scan: one delisted or renamed
# package must not hide every other provider's release. The query therefore must
# not return a non-zero status - the caller assigns the result, and under
# `set -e` a failing assignment would end the whole scan.
npm_view() { # $1=package
  local raw
  raw=$(npm view "$1" version 2>/dev/null) || return 0
  printf '%s\n' "$raw" | tail -n 1 | tr -d '[:space:]'
}

# newest_of prints whichever of two versions is the newer release.
#
# `sort -V` alone is not enough here: it orders a prerelease after the release of
# the same version (`0.2.0-rc.1 > 0.2.0`), so a package pinned at `0.2.0-rc.1`
# would look newer than the `0.2.0` it is waiting for and the upgrade would be
# reported as "pinned ahead" instead of as an available release. Comparing the
# release core first, and preferring the release whenever the cores match, is
# what "is there a newer upstream release?" actually means.
newest_of() { # $1 $2
  local a=$1 b=$2 a_core=${1%%-*} b_core=${2%%-*}
  if [[ $a_core != "$b_core" ]]; then
    if [[ $(printf '%s\n%s\n' "$a_core" "$b_core" | sort -V | tail -n 1) == "$a_core" ]]; then
      printf '%s' "$a"
    else
      printf '%s' "$b"
    fi
    return
  fi
  if [[ $a == "$a_core" || $b == "$b_core" ]]; then
    # Same core: the release outranks its own prereleases. Equal cores with no
    # prerelease are the same version, and the caller checks that case first.
    if [[ $a == "$a_core" ]]; then printf '%s' "$a"; else printf '%s' "$b"; fi
    return
  fi
  # Both are prereleases of the same core; compare the suffixes.
  printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n 1
}

bump_args=''
outdated=false
upgrade_count=0
retired_count=0
unknown_count=0
# The summary lists only what needs attention, so its lines are accumulated next
# to the status they describe instead of being derived from a second pass.
notify_entries=''

while IFS=$'\t' read -r kind npm_package current retirement; do
  latest=$(npm_view "$npm_package")

  if [[ -z $latest ]]; then
    # A delisted or renamed package must not hide every other provider's release,
    # so the failure is counted and the scan continues.
    unknown_count=$((unknown_count + 1))
    continue
  fi
  if [[ $latest == "$current" ]]; then
    continue
  fi
  if [[ $(newest_of "$current" "$latest") != "$latest" ]]; then
    # Pinned ahead of npm latest (prerelease or yanked): not an upgrade, so it is
    # neither reported nor bumped.
    continue
  fi

  outdated=true
  if [[ -n $retirement ]]; then
    # Left out of bump_args on purpose: the lock records that this package is
    # superseded by a newer provider release, so the plan is to delete it in that
    # upgrade rather than to keep chasing its own versions.
    retired_count=$((retired_count + 1))
    notify_entries+="- \`${kind}\`：已被 \`${retirement}\` 取代，随该升级删除"$'\n'
  else
    bump_args+="${kind}=${latest} "
    upgrade_count=$((upgrade_count + 1))
    notify_entries+="- \`${kind}\`：\`${current}\` → \`${latest}\`"$'\n'
  fi
done < <(jq -r '
  .packages
  | to_entries[]
  | [
      .key,
      .value.npm,
      .value.version,
      (if .value.retiredBy then "\(.value.retiredBy.package) \(.value.retiredBy.version)" else "" end)
    ]
  | @tsv
' "$LOCK_FILE")

printf '%s' "${bump_args% }" >"$BUMP_FILE"
if [[ $outdated == true ]]; then
  printf 'true' >"$STATUS_FILE"
else
  printf 'false' >"$STATUS_FILE"
fi
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  printf 'outdated=%s\n' "$outdated" >>"$GITHUB_OUTPUT"
fi

{
  if [[ $outdated == true ]]; then
    printf '有 %d 个上游 provider 有新版本：\n\n' "$((upgrade_count + retired_count))"
    printf '%s' "$notify_entries"
    if ((unknown_count > 0)); then
      # The failure is worth a line of its own: "we could not tell" is not the
      # same as "nothing to do", and a silent omission would read as the latter.
      printf '\n另有 %d 个包查询失败，本次未取到 npm 版本。\n' "$unknown_count"
    fi
  elif ((unknown_count > 0)); then
    # "We could not tell" must not read as "nothing to do", and it must stay
    # true whether one package failed or every one did.
    printf '未发现需要升级的 provider；另有 %d 个包查询失败，本次未取到 npm 版本。\n' "$unknown_count"
  else
    echo '所有固定的 provider 版本都与 npm 最新版本一致，无需处理。'
  fi
} >"$NOTIFY_FILE"

if [[ -n $MENTIONS ]]; then
  printf '\n%s\n' "$MENTIONS" >>"$NOTIFY_FILE"
fi

echo "check-provider-upstreams: outdated=$outdated notify=$NOTIFY_FILE"
