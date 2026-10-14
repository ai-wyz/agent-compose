#!/usr/bin/env bash
#
# Verify that a built guest image really declares the ABI this checkout expects.
#
# The ABI is only "simple and reliable" if something checks it. This reads the
# image label and the manifest baked into the image, and fails when either
# disagrees with guest-images/providers.lock.json - which is the case that would
# otherwise be discovered only after a deployment pointed at the wrong image.
#
# Usage: verify-guest-image-abi.sh <image-ref>
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
PROVIDER_LOCK="$ROOT_DIR/scripts/provider-lock.sh"
MANIFEST_PATH=/etc/agent-compose/guest-abi.json

image=${1:-}
if [[ -z $image ]]; then
  echo 'usage: verify-guest-image-abi.sh <image-ref>' >&2
  exit 2
fi

failures=0
fail() {
  printf 'verify-guest-image-abi: %s\n' "$*" >&2
  failures=$((failures + 1))
}

expected_abi=$("$PROVIDER_LOCK" guest-abi-version)

label_abi=$(docker image inspect --format '{{ index .Config.Labels "io.agent-compose.guest-abi" }}' "$image" 2>/dev/null || true)
if [[ -z $label_abi || $label_abi == '<no value>' ]]; then
  fail "$image carries no io.agent-compose.guest-abi label"
elif [[ $label_abi != "$expected_abi" ]]; then
  fail "$image label guest ABI is '$label_abi', lock says '$expected_abi'"
fi

manifest=$(docker run --rm --entrypoint sh "$image" -lc "cat $MANIFEST_PATH" 2>/dev/null || true)
if [[ -z $manifest ]]; then
  fail "$image does not expose $MANIFEST_PATH"
elif ! jq -e . <<<"$manifest" >/dev/null 2>&1; then
  fail "$MANIFEST_PATH in $image is not valid JSON"
else
  manifest_abi=$(jq -r '.guestAbi' <<<"$manifest")
  if [[ $manifest_abi != "$expected_abi" ]]; then
    fail "$MANIFEST_PATH guest ABI is '$manifest_abi', lock says '$expected_abi'"
  fi

  # A provider version that differs from the lock means the image was built from
  # different pins than the checkout claims, which is exactly the drift the lock
  # exists to prevent. Only packages that ship an agent executable are compared:
  # companions such as pi-mcp-adapter are build inputs, and the manifest
  # deliberately omits them.
  while IFS=$'\t' read -r kind want; do
    got=$(jq -r --arg kind "$kind" '.providers[$kind].version // ""' <<<"$manifest")
    if [[ -z $got ]]; then
      fail "$MANIFEST_PATH declares no version for provider '$kind'"
    elif [[ $got != "$want" ]]; then
      fail "provider '$kind' in $image is '$got', lock says '$want'"
    fi
  done < <(jq -r '
    .packages
    | to_entries[]
    | select(.value.bin != null)
    | [.key, .value.version]
    | @tsv
  ' "$ROOT_DIR/guest-images/providers.lock.json")

  echo "verify-guest-image-abi: $image declares guest ABI $manifest_abi"
  jq -r '.providers | to_entries[] | "  \(.key) \(.value.version)  [\(.value.features | join(", "))]"' <<<"$manifest"
fi

if ((failures > 0)); then
  printf 'verify-guest-image-abi: %d check(s) failed\n' "$failures" >&2
  exit 1
fi
