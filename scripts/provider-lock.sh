#!/usr/bin/env bash
#
# Single source of truth for the agent provider versions baked into a guest
# image, and the guest ABI manifest that image publishes.
#
# `guest-images/providers.lock.json` is the only place a provider version is
# authored. Everything that consumes a version - both guest Dockerfiles'
# ARG defaults, the guest build script, and the generated
# `guest-images/guest-abi.json` - is derived from it, and `check` fails the
# build when one of them drifts.
#
# The guest ABI is deliberately independent of the provider versions: bumping a
# provider leaves `guestAbi` alone, which is what lets a guest image be rebuilt
# and published without a matching daemon release. Bump `guestAbi` only when the
# daemon/guest runtime contract itself changes.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
LOCK_FILE=${PROVIDER_LOCK_FILE:-$ROOT_DIR/guest-images/providers.lock.json}
ABI_FILE=${GUEST_ABI_FILE:-$ROOT_DIR/guest-images/guest-abi.json}

GUEST_DOCKERFILE=$ROOT_DIR/guest-images/Dockerfile.agent-compose-guest
ARCHLINUX_GUEST_DOCKERFILE=$ROOT_DIR/guest-images/Dockerfile.agent-compose-guest-archlinux
GUEST_BUILDER=$ROOT_DIR/scripts/build-agent-compose-guest.sh

die() {
  echo "provider-lock: $*" >&2
  exit 1
}

require_lock() {
  [[ -f $LOCK_FILE ]] || die "lock file not found: $LOCK_FILE"
  jq -e . "$LOCK_FILE" >/dev/null 2>&1 || die "lock file is not valid JSON: $LOCK_FILE"
}

# package_pairs prints every pinned build arg as "NAME=version" in lock order.
package_pairs() {
  jq -r '.packages | to_entries[] | "\(.value.buildArg)=\(.value.version)"' "$LOCK_FILE"
}

# guest_abi_manifest prints the ABI manifest the image publishes. Only agent
# providers appear under "providers"; companion packages are build inputs, not
# guest capabilities, so they are intentionally absent.
guest_abi_manifest() {
  jq '{
    guestAbi: .guestAbi,
    providers: (
      .packages
      | to_entries
      | map(
          select(.value.bin != null)
          | {key: .key, value: {version: .value.version, features: .value.features}}
        )
      | from_entries
    )
  }' "$LOCK_FILE"
}

arg_default() { # $1=file $2=ARG name
  sed -n "s/^ARG[[:space:]]\{1,\}$2=\{0,1\}\(.*\)$/\1/p" "$1" | head -n 1
}

check_dockerfile_arg() { # $1=file $2=ARG name $3=expected value
  local file=$1 name=$2 want=$3 got
  got=$(arg_default "$file" "$name")
  if [[ $got != "$want" ]]; then
    die "$(basename "$file"): ARG $name default is '${got:-<absent>}', lock says '$want'"
  fi
}

check_builder_forwarding() { # $1=ARG name
  grep -Eq "append_build_arg[[:space:]]+$1([[:space:]]|$)" "$GUEST_BUILDER" \
    || die "scripts/build-agent-compose-guest.sh does not forward $1"
}

# newest_version prints the newer release, treating a stable release as newer
# than prereleases with the same release core.
newest_version() { # $1 $2
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
    if [[ $a == "$a_core" ]]; then printf '%s' "$a"; else printf '%s' "$b"; fi
    return
  fi
  printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n 1
}

cmd_pairs() {
  require_lock
  package_pairs
}

cmd_guest_abi() {
  require_lock
  guest_abi_manifest
}

cmd_guest_abi_version() {
  require_lock
  jq -er '.guestAbi' "$LOCK_FILE"
}

cmd_sync() {
  require_lock
  guest_abi_manifest >"$ABI_FILE"
  echo "provider-lock: wrote $ABI_FILE"
}

# check_retirements fails when a package is still pinned next to a release that
# supersedes it. Recording the intent in the lock rather than a comment is what
# makes "drop this when we upgrade" survive the upgrade: the bump that triggers
# it fails until the package is actually removed.
check_retirements() {
  local kind superseded_by superseding_version current reason
  while IFS=$'\t' read -r kind superseded_by superseding_version; do
    [[ -n $kind ]] || continue
    current=$(jq -r --arg kind "$superseded_by" '.packages[$kind].version // ""' "$LOCK_FILE")
    [[ -n $current ]] || continue
    if [[ $(newest_version "$superseding_version" "$current") == "$current" ]]; then
      reason=$(jq -r --arg kind "$kind" '.packages[$kind].retiredBy.reason // ""' "$LOCK_FILE")
      die "package '$kind' is retired by $superseded_by $current (>= $superseding_version): ${reason:-remove it}"
    fi
  done < <(jq -r '
    .packages
    | to_entries[]
    | select(.value.retiredBy != null)
    | [.key, .value.retiredBy.package, .value.retiredBy.version]
    | @tsv
  ' "$LOCK_FILE")
}

cmd_check() {
  require_lock

  check_retirements

  local guest_abi
  guest_abi=$(jq -er '.guestAbi' "$LOCK_FILE")

  [[ -f $ABI_FILE ]] || die "generated manifest not found: $ABI_FILE (run scripts/provider-lock.sh sync)"

  local expected_abi_diff
  expected_abi_diff=$(diff -u "$ABI_FILE" <(guest_abi_manifest) || true)
  if [[ -n $expected_abi_diff ]]; then
    echo "$expected_abi_diff" >&2
    die "guest-images/guest-abi.json is stale; run scripts/provider-lock.sh sync"
  fi

  local line name version
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    name=${line%%=*}
    version=${line#*=}
    check_dockerfile_arg "$GUEST_DOCKERFILE" "$name" "$version"
    check_dockerfile_arg "$ARCHLINUX_GUEST_DOCKERFILE" "$name" "$version"
    check_builder_forwarding "$name"
  done < <(package_pairs)

  check_dockerfile_arg "$GUEST_DOCKERFILE" GUEST_ABI "$guest_abi"
  check_dockerfile_arg "$ARCHLINUX_GUEST_DOCKERFILE" GUEST_ABI "$guest_abi"
  check_builder_forwarding GUEST_ABI

  echo "provider-lock: lock, Dockerfiles, build script, and guest ABI manifest agree"
}

cmd_bump() {
  [[ $# -gt 0 ]] || die "bump expects at least one <provider>=<version>"

  require_lock

  local pair kind version build_arg old escaped_old tmp file
  for pair in "$@"; do
    kind=${pair%%=*}
    version=${pair#*=}
    [[ $kind != "$pair" && -n $version ]] || die "bump expects <provider>=<version>, got '$pair'"
    jq -e --arg kind "$kind" '.packages[$kind]' "$LOCK_FILE" >/dev/null \
      || die "unknown provider '$kind'; add it to the lock before bumping it"

    build_arg=$(jq -r --arg kind "$kind" '.packages[$kind].buildArg' "$LOCK_FILE")
    old=$(jq -r --arg kind "$kind" '.packages[$kind].version' "$LOCK_FILE")
    if [[ $old == "$version" ]]; then
      echo "provider-lock: $kind is already at $version"
      continue
    fi

    # Rewriting every authoring site is the whole point of the lock: a version
    # bump must be mechanical, so the only judgement left is whether the new
    # version passes the guest image build and its acceptance tests.
    tmp=$(mktemp)
    jq --arg kind "$kind" --arg version "$version" '.packages[$kind].version = $version' "$LOCK_FILE" >"$tmp"
    mv "$tmp" "$LOCK_FILE"

    escaped_old=${old//./\\.}
    for file in "$GUEST_DOCKERFILE" "$ARCHLINUX_GUEST_DOCKERFILE"; do
      sed "s|^ARG $build_arg=$escaped_old\$|ARG $build_arg=$version|" "$file" >"$file.bump.tmp"
      mv "$file.bump.tmp" "$file"
    done

    echo "provider-lock: $kind $old -> $version"
  done

  cmd_sync
}

case "${1:-}" in
  pairs) cmd_pairs ;;
  guest-abi) cmd_guest_abi ;;
  guest-abi-version) cmd_guest_abi_version ;;
  sync) cmd_sync ;;
  check) cmd_check ;;
  bump)
    shift
    cmd_bump "$@"
    ;;
  *)
    cat >&2 <<'USAGE'
usage: provider-lock.sh <command>

  pairs              print every pinned build arg as NAME=version, in lock order
  guest-abi          print the guest ABI manifest the image publishes
  guest-abi-version  print the guest ABI contract version
  sync               write guest-images/guest-abi.json from the lock
  check              fail when the Dockerfiles, the build script, or the manifest
                     drift, or when a package is still pinned past the release
                     its retiredBy entry says supersedes it
  bump K=V [K=V ...] rewrite one provider version in the lock, both Dockerfiles,
                     and the generated manifest; does not validate the version
USAGE
    exit 2
    ;;
esac
