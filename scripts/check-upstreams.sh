#!/usr/bin/env bash
#
# Compare every pinned guest provider package against npm and every pinned native
# runtime driver against its GitHub releases, then write the short Simplified
# Chinese summary a scheduled workflow pushes to the development DingTalk group.
#
# This exists so discovering an upstream release stops being a human chore. It
# only reports: it never edits a pin and never merges anything, because deciding
# whether a new provider or driver version is safe is the part that needs a
# person and the acceptance tests that go with it.
#
# Providers are authored in guest-images/providers.lock.json and queried through
# npm. Native runtime drivers are authored in drivers.lock.json and queried
# through the GitHub releases API. Docker and Kubernetes are deliberately absent:
# they are host-provided deployment dependencies rather than artifacts this
# repository builds against a pinned SDK, so there is no version here to chase.
#
# Outputs, under UPSTREAM_OUTPUT_DIR (default: the current directory):
#   upstream-notify.md     the summary pushed to DingTalk
#   upstream-bump.txt      "kind=version" arguments for provider-lock.sh bump
#   upstream-outdated      "true" or "false"
#
# The summary is Simplified Chinese because it is read in a DingTalk group by the
# development team. It lists only what needs attention: which pinned providers or
# drivers have a newer release upstream, which providers are retired by another
# provider's release, and how many upstream queries failed. A version that is up
# to date, or pinned ahead of upstream, is deliberately not listed.
#
# When GITHUB_OUTPUT is set, "outdated=<bool>" and "bump=<bool>" are appended.
# "bump" is true only when provider pins are outdated: driver bumps are not
# mechanical, so the draft bump pull request never carries them.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PROVIDER_LOCK=${PROVIDER_LOCK_FILE:-$ROOT_DIR/guest-images/providers.lock.json}
DRIVER_LOCK=${DRIVER_LOCK_FILE:-$ROOT_DIR/drivers.lock.json}
DRIVER_DOCKERFILE=${DRIVER_DOCKERFILE:-$ROOT_DIR/Dockerfile}
GO_MOD_FILE=${GO_MOD_FILE:-$ROOT_DIR/go.mod}
OUTPUT_DIR=${UPSTREAM_OUTPUT_DIR:-$PWD}
MENTIONS=${PROVIDER_UPGRADE_MENTIONS:-}

NOTIFY_FILE="$OUTPUT_DIR/upstream-notify.md"
BUMP_FILE="$OUTPUT_DIR/upstream-bump.txt"
STATUS_FILE="$OUTPUT_DIR/upstream-outdated"

die() {
  echo "check-upstreams: $*" >&2
  exit 1
}

arg_default() { # $1=file $2=ARG name
  sed -n "s/^ARG[[:space:]]\{1,\}$2=\{0,1\}\(.*\)$/\1/p" "$1" | head -n 1
}

go_module_version() { # $1=go.mod $2=module path
  awk -v module="$2" '$1 == module { print $2; exit }' "$1"
}

require_provider_lock() {
  [[ -f $PROVIDER_LOCK ]] || die "provider lock file not found: $PROVIDER_LOCK"
  jq -e '.packages | type == "object"' "$PROVIDER_LOCK" >/dev/null 2>&1 ||
    die "provider lock file is not a package lock: $PROVIDER_LOCK"
}

require_driver_lock() {
  [[ -f $DRIVER_LOCK ]] || die "driver lock file not found: $DRIVER_LOCK"
  jq -e '
    .drivers
    | type == "object" and length > 0 and
      all(.[];
        (.repo | type == "string" and length > 0) and
        (.version | type == "string" and length > 0) and
        (.buildArg | type == "string" and length > 0) and
        ((.goModule // "") | type == "string")
      )
  ' "$DRIVER_LOCK" >/dev/null 2>&1 ||
    die "driver lock file must map each driver to repo, version, and buildArg: $DRIVER_LOCK"
}

# check_driver_pins keeps the lock honest. The scan reports the lock's versions,
# but the Dockerfile ARG is what the image actually builds, and the Microsandbox
# FFI assets are kept in lockstep with the Go SDK module. A stale lock would
# therefore report the wrong upstream comparison, so refuse to scan it.
check_driver_pins() {
  local kind build_arg version go_module got
  while IFS=$'\t' read -r kind build_arg version go_module; do
    got=$(arg_default "$DRIVER_DOCKERFILE" "$build_arg")
    if [[ $got != "$version" ]]; then
      die "$(basename "$DRIVER_DOCKERFILE"): ARG $build_arg default is '${got:-<absent>}', drivers.lock.json pins $kind at '$version'"
    fi
    [[ -n $go_module ]] || continue
    got=$(go_module_version "$GO_MOD_FILE" "$go_module")
    if [[ -z $got ]]; then
      die "$(basename "$GO_MOD_FILE") does not require $go_module, which drivers.lock.json pins for $kind"
    fi
    if [[ ${got#v} != "${version#v}" ]]; then
      die "$(basename "$GO_MOD_FILE") pins $go_module at '$got', drivers.lock.json pins $kind at '$version'"
    fi
  done < <(jq -r '
    .drivers
    | to_entries[]
    | [.key, .value.buildArg, .value.version, (.value.goModule // "")]
    | @tsv
  ' "$DRIVER_LOCK")
}

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

# github_latest_tag reads the tag of the newest published stable release. A
# failure is reported as "unknown" for the same reason as npm_view: one
# repository with an API error or no stable release must not hide every other
# driver's release.
github_latest_tag() { # $1=owner/repo
  local api_url=${UPSTREAM_GITHUB_API_URL:-https://api.github.com}
  local url="$api_url/repos/$1/releases/latest"
  local raw
  local -a curl_args=(
    -fsSL
    --retry 3 --retry-all-errors --retry-delay 2
    --connect-timeout 10 --max-time 30
    -H 'Accept: application/vnd.github+json'
    -H 'X-GitHub-Api-Version: 2022-11-28'
  )

  if [[ -n ${GITHUB_TOKEN:-} ]]; then
    curl_args+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi
  raw=$(curl "${curl_args[@]}" "$url" 2>/dev/null) || return 0
  printf '%s\n' "$raw" | jq -r '.tag_name // empty' 2>/dev/null || true
}

strip_v() {
  printf '%s' "${1#v}"
}

# newest_of prints whichever of two versions is the newer release. GitHub release
# tags carry a leading "v" while npm versions and the Go module do not, so the
# prefix is stripped before comparing.
#
# `sort -V` alone is not enough here: it orders a prerelease after the release of
# the same version (`0.2.0-rc.1 > 0.2.0`), so a package pinned at `0.2.0-rc.1`
# would look newer than the `0.2.0` it is waiting for and the upgrade would be
# reported as "pinned ahead" instead of as an available release. Comparing the
# release core first, and preferring the release whenever the cores match, is
# what "is there a newer upstream release?" actually means.
newest_of() { # $1 $2
  local a b a_core b_core
  a=$(strip_v "$1")
  b=$(strip_v "$2")
  a_core=${a%%-*}
  b_core=${b%%-*}
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

require_provider_lock
require_driver_lock
check_driver_pins

mkdir -p "$OUTPUT_DIR"

bump_args=''
outdated=false
provider_count=0
driver_count=0
unknown_count=0
# The summary lists only what needs attention, so its lines are accumulated next
# to the status they describe instead of being derived from a second pass.
provider_entries=''
driver_entries=''

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
    provider_count=$((provider_count + 1))
    provider_entries+="- \`${kind}\`：已被 \`${retirement}\` 取代，随该升级删除"$'\n'
  else
    bump_args+="${kind}=${latest} "
    provider_count=$((provider_count + 1))
    provider_entries+="- \`${kind}\`：\`${current}\` → \`${latest}\`"$'\n'
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
' "$PROVIDER_LOCK")

while IFS=$'\t' read -r kind repo current; do
  latest=$(github_latest_tag "$repo")

  if [[ -z $latest ]]; then
    unknown_count=$((unknown_count + 1))
    continue
  fi
  if [[ $latest == "$current" ]]; then
    continue
  fi
  if [[ $(newest_of "$current" "$latest") != "$(strip_v "$latest")" ]]; then
    # Pinned ahead of the latest stable release, or upstream published only a
    # prerelease: not an upgrade, so it is neither reported nor bumped.
    continue
  fi

  outdated=true
  driver_count=$((driver_count + 1))
  driver_entries+="- \`${kind}\`：\`${current}\` → \`${latest}\`"$'\n'
done < <(jq -r '
  .drivers
  | to_entries[]
  | [.key, .value.repo, .value.version]
  | @tsv
' "$DRIVER_LOCK")

if [[ -n $bump_args ]]; then
  bump=true
  printf '%s' "${bump_args% }" >"$BUMP_FILE"
else
  bump=false
  : >"$BUMP_FILE"
fi
if [[ $outdated == true ]]; then
  printf 'true' >"$STATUS_FILE"
else
  printf 'false' >"$STATUS_FILE"
fi
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  printf 'outdated=%s\nbump=%s\n' "$outdated" "$bump" >>"$GITHUB_OUTPUT"
fi

{
  if [[ $outdated == true ]]; then
    if ((provider_count > 0 && driver_count > 0)); then
      printf '有 %d 个上游依赖有新版本（provider %d 个，driver %d 个）：\n\n' \
        "$((provider_count + driver_count))" "$provider_count" "$driver_count"
    elif ((driver_count > 0)); then
      printf '有 %d 个上游 driver 有新版本：\n\n' "$driver_count"
    else
      printf '有 %d 个上游 provider 有新版本：\n\n' "$provider_count"
    fi

    if ((provider_count > 0)); then
      if ((driver_count > 0)); then
        printf '**Provider**\n\n%s\n' "$provider_entries"
      else
        printf '%s' "$provider_entries"
      fi
    fi
    if ((driver_count > 0)); then
      if ((provider_count > 0)); then
        printf '**Driver**\n\n%s\n' "$driver_entries"
      else
        printf '%s' "$driver_entries"
      fi
    fi

    if ((unknown_count > 0)); then
      # The failure is worth a line of its own: "we could not tell" is not the
      # same as "nothing to do", and a silent omission would read as the latter.
      printf '\n另有 %d 个上游查询失败，本次未取到最新版本。\n' "$unknown_count"
    fi
  elif ((unknown_count > 0)); then
    # "We could not tell" must not read as "nothing to do", and it must stay
    # true whether one query failed or every one did.
    printf '未发现需要升级的上游依赖；另有 %d 个上游查询失败，本次未取到最新版本。\n' "$unknown_count"
  else
    echo '所有固定的 provider 与 driver 版本都与上游最新版本一致，无需处理。'
  fi
} >"$NOTIFY_FILE"

if [[ -n $MENTIONS ]]; then
  printf '\n%s\n' "$MENTIONS" >>"$NOTIFY_FILE"
fi

echo "check-upstreams: outdated=$outdated bump=$bump notify=$NOTIFY_FILE"
