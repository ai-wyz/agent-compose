#!/usr/bin/env bash
#
# Compare every pinned guest provider package against the version npm currently
# publishes, and write a Markdown report a scheduled workflow pushes to the team
# webhook.
#
# This exists so discovering an upstream release stops being a human chore. It
# only reports: it never edits a pin and never merges anything, because deciding
# whether a new provider version is safe is the part that needs a person and the
# guest image's acceptance tests.
#
# Outputs, under PROVIDER_UPSTREAM_OUTPUT_DIR (default: the current directory):
#   provider-upstream-report.md     the Markdown report
#   provider-upstream-notify.md     the short Simplified Chinese push summary
#   provider-upstream-bump.txt      "kind=version" arguments for provider-lock.sh bump
#   provider-upstream-outdated      "true" or "false"
#
# When GITHUB_OUTPUT is set, "outdated=<bool>" is also appended to it.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
LOCK_FILE=${PROVIDER_LOCK_FILE:-$ROOT_DIR/guest-images/providers.lock.json}
OUTPUT_DIR=${PROVIDER_UPSTREAM_OUTPUT_DIR:-$PWD}
MENTIONS=${PROVIDER_UPGRADE_MENTIONS:-}

REPORT_FILE="$OUTPUT_DIR/provider-upstream-report.md"
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

rows=''
bump_args=''
outdated=false
upgrade_count=0
retired_count=0
unknown_count=0
# The push summary is Simplified Chinese while the report stays English: the
# report is the repository's durable record and the pull request body, whereas
# the push is read by the development team in a DingTalk group. It is built here,
# next to the status it summarizes, so the two cannot disagree about a package.
notify_entries=''

while IFS=$'\t' read -r kind npm_package current retirement; do
  latest=$(npm_view "$npm_package")

  if [[ -z $latest ]]; then
    status='unknown (npm query failed)'
    unknown_count=$((unknown_count + 1))
  elif [[ $latest == "$current" ]]; then
    status='up to date'
  elif [[ $(newest_of "$current" "$latest") == "$latest" ]]; then
    outdated=true
    if [[ -n $retirement ]]; then
      # Left out of bump_args on purpose: the lock records that this package is
      # superseded by a newer provider release, so the plan is to delete it in
      # that upgrade rather than to keep chasing its own versions.
      status="**outdated** — retired by \`${retirement}\`; remove it instead of bumping"
      retired_count=$((retired_count + 1))
      notify_entries+="- \`${kind}\`：已被 \`${retirement}\` 取代，随该升级删除"$'\n'
    else
      status='**outdated**'
      bump_args+="${kind}=${latest} "
      upgrade_count=$((upgrade_count + 1))
      notify_entries+="- \`${kind}\`：\`${current}\` → \`${latest}\`"$'\n'
    fi
  else
    status='pinned ahead of npm latest (prerelease or yanked)'
  fi

  rows+="| \`${kind}\` | \`${npm_package}\` | \`${current}\` | \`${latest:-?}\` | ${status} |"$'\n'
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
  echo '# Upstream provider releases pending'
  echo
  if [[ $outdated == true ]]; then
    echo 'A guest provider package has a newer release on npm than this repository pins.'
  else
    echo 'Every pinned guest provider package matches the version npm currently publishes.'
  fi
  echo
  echo '| Provider | Package | Pinned | npm latest | Status |'
  echo '| --- | --- | --- | --- | --- |'
  printf '%s' "$rows"
  echo
  echo '## Acting on this'
  echo
  echo 'The bump itself is mechanical: `scripts/provider-lock.sh bump` rewrites the'
  echo 'lock, both guest Dockerfiles, and the generated ABI manifest together, and'
  echo '`scripts/provider-lock.sh check` fails when any of them drift.'
  echo
  echo '```bash'
  echo "scripts/provider-lock.sh bump ${bump_args% }"
  echo 'task image:agent-compose-guest'
  echo '```'
  echo
  echo '**Bumping is not validating.** A provider release changes how the CLI'
  echo 'behaves, and only the guest image build plus the provider acceptance run'
  echo 'show whether the runtime adapter still understands it. Treat the pinned'
  echo 'versions as a reviewed decision, not as something to chase automatically.'
  echo
  echo 'A provider-only bump must not change `guestAbi`, so the rebuilt guest image'
  echo 'stays compatible with an already-deployed daemon. If you find yourself'
  echo 'needing to raise `guestAbi`, the change is a contract change and needs a'
  echo 'daemon release too.'
  echo
  echo "---"
  echo "Generated by \`scripts/check-provider-upstreams.sh\`."
} >"$REPORT_FILE"

{
  if [[ $outdated == true ]]; then
    printf '有 %d 个上游 provider 有新版本：\n\n' "$((upgrade_count + retired_count))"
    printf '%s' "$notify_entries"
    echo
    if ((unknown_count > 0)); then
      printf '另有 %d 个包查询失败，详情见完整报告。\n\n' "$unknown_count"
    fi
    echo '完整报告、升级命令和验证步骤见本次 GitHub Actions 运行的 Summary。'
  elif ((unknown_count > 0)); then
    printf '有 %d 个包查询失败，其余固定版本与 npm 最新版本一致，详情见完整报告。\n' "$unknown_count"
  else
    echo '所有固定的 provider 版本都与 npm 最新版本一致，无需处理。'
  fi
} >"$NOTIFY_FILE"

if [[ -n $MENTIONS ]]; then
  printf '\n%s\n' "$MENTIONS" >>"$REPORT_FILE"
  printf '\n%s\n' "$MENTIONS" >>"$NOTIFY_FILE"
fi

echo "check-provider-upstreams: outdated=$outdated report=$REPORT_FILE notify=$NOTIFY_FILE"
