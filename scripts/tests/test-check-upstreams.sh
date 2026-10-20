#!/usr/bin/env bash
#
# Contract checks for scripts/check-upstreams.sh.
#
# The daily scan is the only thing that reports an upstream provider or driver
# release, and the Chinese summary it writes for the DingTalk push is the only
# copy anyone reads, so the summary's content and the scan's silent-failure modes
# are the behavior worth pinning down.
#
# Every case runs against its own fixture locks and stubbed `npm` and `curl`, so
# the cases neither touch the network nor depend on the versions currently pinned
# in the committed lock files. A provider or driver bump is the routine this check
# exists to support; it must not be able to fail the check.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
CHECK="$ROOT_DIR/scripts/check-upstreams.sh"
REAL_PROVIDER_LOCK="$ROOT_DIR/guest-images/providers.lock.json"
REAL_DRIVER_LOCK="$ROOT_DIR/drivers.lock.json"
REAL_DOCKERFILE="$ROOT_DIR/Dockerfile"
REAL_GO_MOD="$ROOT_DIR/go.mod"

failures=0
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT

fail() {
  printf 'test-check-upstreams: %s\n' "$*" >&2
  failures=$((failures + 1))
}

notify() { cat "$scan_dir/out/upstream-notify.md" 2>/dev/null || true; }
bump_args() { cat "$scan_dir/out/upstream-bump.txt" 2>/dev/null || true; }
status() { cat "$scan_dir/out/upstream-outdated" 2>/dev/null || true; }

# visited_providers and visited_drivers count the queries each stub answered. The
# summary lists only what needs attention, so how far the scan got is not visible
# in its output; the answered queries are the proof that a scan covered a whole
# lock even when one query failed.
visited_providers() {
  if [[ -f $scan_dir/npm_queries.log ]]; then
    wc -l <"$scan_dir/npm_queries.log" | tr -d ' '
  else
    printf '0'
  fi
}

visited_drivers() {
  if [[ -f $scan_dir/curl_queries.log ]]; then
    wc -l <"$scan_dir/curl_queries.log" | tr -d ' '
  else
    printf '0'
  fi
}

npm_stub_answer() { # $1=pinned version $2=stub behaviour
  case $2 in
    same) printf 'echo %s' "$1" ;;
    '!fail') printf 'exit 1' ;;
    *) printf 'echo %s' "$2" ;;
  esac
}

curl_stub_answer() { # $1=pinned tag $2=stub behaviour
  local tag
  case $2 in
    same) tag=$1 ;;
    '!fail') printf 'exit 22'; return ;;
    *) tag=$2 ;;
  esac
  printf "echo '{\"tag_name\":\"%s\"}'" "$tag"
}

# write_stubs installs a fake npm and curl. npm answers "view <package> version"
# from the case's npm_arms and records every queried package; curl resolves the
# GitHub releases URL from the case's curl_arms and records every requested URL.
write_stubs() { # $1=case directory
  local dir=$1
  mkdir -p "$dir/bin"

  {
    echo '#!/usr/bin/env bash'
    printf 'echo "$2" >>%q\n' "$dir/npm_queries.log"
    echo 'case "$2" in'
    cat "$dir/npm_arms"
    echo '  *) echo "" ;;'
    echo 'esac'
  } >"$dir/bin/npm"
  chmod +x "$dir/bin/npm"

  {
    echo '#!/usr/bin/env bash'
    echo 'url="${!#}"'
    printf 'printf "%%s\\n" "$url" >>%q\n' "$dir/curl_queries.log"
    echo 'case "$url" in'
    cat "$dir/curl_arms"
    echo '  *) echo "curl: (22) The requested URL returned error: 404" >&2; exit 22 ;;'
    echo 'esac'
  } >"$dir/bin/curl"
  chmod +x "$dir/bin/curl"
}

# run_scan runs the check with the case's stubs and records the outcome in
# scan_rc. Remaining arguments are environment assignments that override the
# fixture paths, which is how the committed locks are scanned too.
run_scan() { # $1=case directory, then optional env assignments
  local dir=$1
  shift
  mkdir -p "$dir/out"
  : >"$dir/github_output"
  set +e
  env PATH="$dir/bin:$PATH" \
    PROVIDER_LOCK_FILE="$dir/lock.json" \
    DRIVER_LOCK_FILE="$dir/drivers.lock.json" \
    DRIVER_DOCKERFILE="$dir/Dockerfile" \
    GO_MOD_FILE="$dir/go.mod" \
    UPSTREAM_OUTPUT_DIR="$dir/out" \
    PROVIDER_UPGRADE_MENTIONS= \
    UPSTREAM_GITHUB_API_URL=https://api.github.test \
    GITHUB_TOKEN= \
    GITHUB_OUTPUT="$dir/github_output" \
    "$@" \
    "$CHECK" >"$dir/stdout" 2>&1
  scan_rc=$?
  set -e
}

# scan builds fixtures for one case and runs the check. The spec arrives on stdin:
#   provider|<kind>|<npm package>|<pinned>|<stub>|<retiredBy>
#   driver|<kind>|<owner/repo>|<pinned>|<stub>|<goModule>
# where <stub> is "same" to answer with the pinned version, "!fail" to make the
# query fail, or a version to report instead. A driver's buildArg is
# "<KIND>_VERSION"; a non-empty goModule adds a matching require line to the
# fixture go.mod. It sets scan_dir (the case directory holding the outputs) and
# scan_rc.
scan() { # $1=case name, spec on stdin
  local type kind package pinned stub extra
  scan_dir="$TEST_ROOT/$1"
  rm -rf "$scan_dir"
  mkdir -p "$scan_dir"
  : >"$scan_dir/npm_arms"
  : >"$scan_dir/curl_arms"
  printf 'module example.test\n\ngo 1.26.2\n' >"$scan_dir/go.mod"
  : >"$scan_dir/Dockerfile"

  local provider_entries='' driver_entries=''
  while IFS='|' read -r type kind package pinned stub extra; do
    [[ -n $type ]] || continue
    case $type in
      provider)
        local retired_by=null
        if [[ -n $extra ]]; then
          retired_by=$(printf '{"package":"%s","version":"%s"}' "${extra%@*}" "${extra#*@}")
        fi
        provider_entries+=$(printf '"%s":{"npm":"%s","version":"%s","bin":"%s","retiredBy":%s},' \
          "$kind" "$package" "$pinned" "$kind" "$retired_by")
        printf '  %s) %s ;;\n' "$package" "$(npm_stub_answer "$pinned" "$stub")" >>"$scan_dir/npm_arms"
        ;;
      driver)
        local build_arg="${kind^^}_VERSION"
        local go_field=''
        if [[ -n $extra ]]; then
          go_field=$(printf ',"goModule":"%s"' "$extra")
          printf 'require (\n\t%s %s\n)\n' "$extra" "${pinned#v}" >>"$scan_dir/go.mod"
        fi
        driver_entries+=$(printf '"%s":{"repo":"%s","version":"%s","buildArg":"%s"%s},' \
          "$kind" "$package" "$pinned" "$build_arg" "$go_field")
        printf 'ARG %s=%s\n' "$build_arg" "$pinned" >>"$scan_dir/Dockerfile"
        printf '  */repos/%s/releases/latest) %s ;;\n' "$package" "$(curl_stub_answer "$pinned" "$stub")" >>"$scan_dir/curl_arms"
        ;;
      *) fail "unknown spec line type '$type'" ;;
    esac
  done

  printf '{"guestAbi":1,"nodeMajor":22,"packages":{%s}}\n' "${provider_entries%,}" >"$scan_dir/lock.json"
  printf '{"drivers":{%s}}\n' "${driver_entries%,}" >"$scan_dir/drivers.lock.json"
  # A malformed fixture would make every assertion below vacuous.
  jq -e . "$scan_dir/lock.json" >/dev/null
  jq -e . "$scan_dir/drivers.lock.json" >/dev/null

  write_stubs "$scan_dir"
  run_scan "$scan_dir"
}

# 1. One failing npm query must not end the scan, and it must not read as "there
#    is nothing to do": the summary says how many queries failed while the scan
#    keeps going, because a single delisted package would otherwise hide every
#    other provider's release. The failing package is listed first.
scan failing-provider-query <<'EOF'
provider|codex|@openai/codex|0.146.0|!fail|
provider|claude|@anthropic-ai/claude-code|2.1.220|same|
provider|dsh|@deepseek-ai/dsh|0.3.0-rc.1|same|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

if ((scan_rc != 0)); then
  fail "a failing npm query aborted the scan (exit $scan_rc)"
fi
if [[ $(visited_providers) != '3' ]]; then
  fail "a failing npm query stopped the scan after $(visited_providers) of 3 packages"
fi
if ! grep -qF '另有 1 个上游查询失败' <(notify); then
  fail "the pushed summary hid a failed npm query: $(notify)"
fi
if grep -q '^- ' <(notify); then
  fail "a scan with no newer release pushed provider bullets: $(notify)"
fi
if [[ -n $(bump_args) ]]; then
  fail "a scan with no newer release produced bump arguments: $(bump_args)"
fi
if [[ $(status) != 'false' ]]; then
  fail "a scan with no newer release reported outdated='$(status)'"
fi
if ! grep -q '^outdated=false$' "$scan_dir/github_output"; then
  fail 'the GITHUB_OUTPUT status was not written for a current set of pins'
fi
if ! grep -q '^bump=false$' "$scan_dir/github_output"; then
  fail 'the GITHUB_OUTPUT bump status was not written for a current set of pins'
fi

# 2. A final release must be reported while a prerelease of it is pinned. GNU
#    version sort orders 0.2.0-rc.1 after 0.2.0, so this is exactly the upgrade
#    the plain comparison used to hide as "pinned ahead".
scan prerelease-pin <<'EOF'
provider|claude|@anthropic-ai/claude-code|2.1.220|same|
provider|dsh|@deepseek-ai/dsh|0.2.0-rc.1|0.2.0|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

if ((scan_rc != 0)); then
  fail "the scan failed for a pinned prerelease (exit $scan_rc)"
fi
if ! grep -qF '有 1 个上游 provider 有新版本' <(notify); then
  fail "the pushed summary did not count the available release: $(notify)"
fi
if ! grep -qF -- '- `dsh`：`0.2.0-rc.1` → `0.2.0`' <(notify); then
  fail "the pushed summary did not name the available upgrade: $(notify)"
fi
# The summary is the list of what to look at, not a table of the whole lock.
if grep -qF 'claude' <(notify); then
  fail "the pushed summary listed a package that needs no attention: $(notify)"
fi
if ! grep -q 'dsh=0.2.0' <(bump_args); then
  fail "the final release was not offered as a bump: $(bump_args)"
fi
if [[ $(status) != 'true' ]]; then
  fail "an available release reported outdated='$(status)'"
fi
if ! grep -q '^bump=true$' "$scan_dir/github_output"; then
  fail 'the GITHUB_OUTPUT bump status was not written for an available release'
fi

# 3. A prerelease upstream of the release we already pin is not an upgrade: the
#    reader must not be told to look at it, and it must stay out of the bump
#    arguments. This is the other half of case 2.
scan prerelease-upstream <<'EOF'
provider|dsh|@deepseek-ai/dsh|0.2.0|0.2.0-rc.1|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

if ! grep -qF '所有固定的 provider 与 driver 版本都与上游最新版本一致，无需处理。' <(notify); then
  fail "a prerelease upstream was reported as an upgrade: $(notify)"
fi
if [[ -n $(bump_args) ]]; then
  fail "a prerelease upstream produced bump arguments: $(bump_args)"
fi
if [[ $(status) != 'false' ]]; then
  fail "a prerelease upstream reported outdated='$(status)'"
fi

# 4. A newer release core must preserve its prerelease suffix when returned by
#    newest_of, so the summary still names that upstream version and the scan
#    still offers it as a bump.
scan newer-prerelease-core <<'EOF'
provider|dsh|@deepseek-ai/dsh|0.2.0|1.0.0-rc.1|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

if ((scan_rc != 0)); then
  fail "the scan failed for a newer prerelease core (exit $scan_rc)"
fi
if ! grep -qF -- '- `dsh`：`0.2.0` → `1.0.0-rc.1`' <(notify); then
  fail "a newer prerelease core was not named in the summary: $(notify)"
fi
if ! grep -q 'dsh=1.0.0-rc.1' <(bump_args); then
  fail "a newer prerelease core was not offered as a bump: $(bump_args)"
fi

# 5. A retired companion is reported so someone can act, but it is never offered
#    as a bump: the recorded plan is to delete it in the upgrade that retires it.
#    The summary has to say which of those two a package is.
scan retired-companion <<'EOF'
provider|opencode|opencode-ai|1.18.9|1.19.0|
provider|pi|@earendil-works/pi-coding-agent|0.82.1|same|
provider|pi-mcp-adapter|pi-mcp-adapter|2.11.0|5.3.0|pi@0.99.0
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

if ! grep -qF '有 2 个上游 provider 有新版本' <(notify); then
  fail "the pushed summary miscounted the providers to look at: $(notify)"
fi
if ! grep -qF -- '- `opencode`：`1.18.9` → `1.19.0`' <(notify); then
  fail "the pushed summary dropped an ordinary outdated provider: $(notify)"
fi
if ! grep -qF -- '- `pi-mcp-adapter`：已被 `pi 0.99.0` 取代，随该升级删除' <(notify); then
  fail "the pushed summary did not say a retired companion is deleted, not bumped: $(notify)"
fi
if grep -q 'pi-mcp-adapter' <(bump_args); then
  fail "a retired companion was offered as a bump: $(bump_args)"
fi
if ! grep -q 'opencode=1.19.0' <(bump_args); then
  fail "an ordinary outdated provider was dropped from the bump list: $(bump_args)"
fi
if [[ $(status) != 'true' ]]; then
  fail "a retired companion reported outdated='$(status)'"
fi

# 6. Providers and drivers share one summary and one outdated flag, but only the
#    provider pins are offered for the draft bump pull request. Drivers are
#    reported for a person to upgrade by hand.
scan provider-and-driver <<'EOF'
provider|opencode|opencode-ai|1.18.9|1.19.0|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|v0.10.7|
driver|microsandbox|superradcompany/microsandbox|v0.7.3|same|github.com/superradcompany/microsandbox/sdk/go
EOF

if ((scan_rc != 0)); then
  fail "the combined provider and driver scan failed (exit $scan_rc)"
fi
if ! grep -qF '有 2 个上游依赖有新版本（provider 1 个，driver 1 个）：' <(notify); then
  fail "the summary did not count providers and drivers: $(notify)"
fi
if ! grep -qF '**Provider**' <(notify); then
  fail "the combined summary dropped its provider section: $(notify)"
fi
if ! grep -qF '**Driver**' <(notify); then
  fail "the combined summary dropped its driver section: $(notify)"
fi
if ! grep -qF -- '- `opencode`：`1.18.9` → `1.19.0`' <(notify); then
  fail "the combined summary dropped the provider upgrade: $(notify)"
fi
if ! grep -qF -- '- `boxlite`：`v0.10.4` → `v0.10.7`' <(notify); then
  fail "the combined summary dropped the driver upgrade: $(notify)"
fi
if grep -qF 'microsandbox' <(notify); then
  fail "the combined summary listed an up-to-date driver: $(notify)"
fi
if [[ $(bump_args) != 'opencode=1.19.0' ]]; then
  fail "the combined summary offered a driver as a provider bump: $(bump_args)"
fi
if [[ $(status) != 'true' ]] || ! grep -q '^bump=true$' "$scan_dir/github_output"; then
  fail "the combined scan did not report outdated and bumpable pins (status='$(status)')"
fi

# 7. Driver-only updates still notify, but there is no provider bump to open a
#    pull request for. This is the case the workflow's bump step must skip rather
#    than treating as a broken scan.
scan driver-only <<'EOF'
provider|opencode|opencode-ai|1.18.9|same|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|v0.10.7|
EOF

if ! grep -qF '有 1 个上游 driver 有新版本：' <(notify); then
  fail "a driver-only update was not counted as a driver update: $(notify)"
fi
if ! grep -qF -- '- `boxlite`：`v0.10.4` → `v0.10.7`' <(notify); then
  fail "a driver-only update was not named: $(notify)"
fi
if grep -qF '**Provider**' <(notify); then
  fail "a driver-only update printed an empty provider section: $(notify)"
fi
if [[ -n $(bump_args) ]]; then
  fail "a driver-only update produced provider bump arguments: $(bump_args)"
fi
if [[ $(status) != 'true' ]] || ! grep -q '^bump=false$' "$scan_dir/github_output"; then
  fail "a driver-only update was not reportable without a bump (status='$(status)')"
fi

# 8. A driver pinned ahead of the latest stable release is not an upgrade. The
#    leading "v" on GitHub tags must not turn it into one.
scan driver-ahead <<'EOF'
provider|opencode|opencode-ai|1.18.9|same|
driver|boxlite|boxlite-ai/boxlite|v0.11.0|v0.10.7|
EOF

if ! grep -qF '所有固定的 provider 与 driver 版本都与上游最新版本一致，无需处理。' <(notify); then
  fail "a driver pinned ahead of upstream was reported as an upgrade: $(notify)"
fi
if [[ $(status) != 'false' ]]; then
  fail "a driver pinned ahead of upstream reported outdated='$(status)'"
fi

# 9. One failing GitHub release query must not hide another driver's release, and
#    it must not abort the provider scan either.
scan failing-driver-query <<'EOF'
provider|codex|@openai/codex|0.146.0|same|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|!fail|
driver|microsandbox|superradcompany/microsandbox|v0.7.3|v0.7.8|
EOF

if ((scan_rc != 0)); then
  fail "a failing GitHub release query aborted the scan (exit $scan_rc)"
fi
if [[ $(visited_drivers) != '2' ]]; then
  fail "a failing GitHub release query stopped the driver scan after $(visited_drivers) of 2 drivers"
fi
if ! grep -qF -- '- `microsandbox`：`v0.7.3` → `v0.7.8`' <(notify); then
  fail "a failing driver query hid another driver's release: $(notify)"
fi
if ! grep -qF '另有 1 个上游查询失败' <(notify); then
  fail "the summary hid a failed GitHub release query: $(notify)"
fi

# 10. Both query kinds can fail without turning "we could not tell" into "there
#     is nothing to do".
scan failing-both-queries <<'EOF'
provider|codex|@openai/codex|0.146.0|!fail|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|!fail|
EOF

if ! grep -qF '未发现需要升级的上游依赖；另有 2 个上游查询失败' <(notify); then
  fail "the summary hid failed provider and driver queries: $(notify)"
fi
if [[ $(status) != 'false' ]]; then
  fail "failed queries reported outdated='$(status)'"
fi

# 11. The driver lock must not drift from what the image actually builds. A stale
#     lock would report a comparison against a version that is not pinned, so the
#     scan refuses to run instead of producing a misleading summary.
scan driver-lock-dockerfile-drift <<'EOF'
provider|opencode|opencode-ai|1.18.9|same|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

printf 'ARG BOXLITE_VERSION=v0.9.0\n' >"$scan_dir/Dockerfile"
run_scan "$scan_dir"
if ((scan_rc == 0)); then
  fail 'the scan accepted a driver lock that disagrees with the Dockerfile'
elif ! grep -q 'BOXLITE_VERSION' "$scan_dir/stdout"; then
  fail 'the Dockerfile drift error did not name the build argument'
fi

# 12. Microsandbox release assets and the Go SDK module are kept in lockstep, so
#     a lock that disagrees with go.mod is drift too.
scan driver-lock-go-module-drift <<'EOF'
provider|opencode|opencode-ai|1.18.9|same|
driver|microsandbox|superradcompany/microsandbox|v0.7.3|same|github.com/superradcompany/microsandbox/sdk/go
EOF

printf 'module example.test\n\ngo 1.26.2\n\nrequire (\n\tgithub.com/superradcompany/microsandbox/sdk/go v0.6.0\n)\n' >"$scan_dir/go.mod"
run_scan "$scan_dir"
if ((scan_rc == 0)); then
  fail 'the scan accepted a driver lock that disagrees with the Go SDK module'
elif ! grep -q 'microsandbox/sdk/go' "$scan_dir/stdout"; then
  fail 'the Go module drift error did not name the SDK module'
fi

# 13. A lock the scan cannot interpret must fail loudly rather than silently scan
#     zero drivers.
scan malformed-driver-lock <<'EOF'
provider|opencode|opencode-ai|1.18.9|same|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF

printf '{"drivers":{"boxlite":{"version":"v0.10.4"}}}\n' >"$scan_dir/drivers.lock.json"
run_scan "$scan_dir"
if ((scan_rc == 0)); then
  fail 'the scan accepted a driver lock without a repo'
elif ! grep -q 'driver lock file' "$scan_dir/stdout"; then
  fail 'the malformed driver lock error did not name the driver lock'
fi

# 14. The committed locks and pins must stay scannable. The stubs echo each
#     package's or driver's own pin, read from the locks, so a bump keeps this
#     case passing while a lock the scan cannot read still fails it.
scan_dir="$TEST_ROOT/committed-locks"
rm -rf "$scan_dir"
mkdir -p "$scan_dir"
: >"$scan_dir/npm_arms"
: >"$scan_dir/curl_arms"
jq -r '.packages | to_entries[] | "  \(.value.npm)) echo \(.value.version) ;;"' \
  "$REAL_PROVIDER_LOCK" >"$scan_dir/npm_arms"
while IFS=$'\t' read -r repo version; do
  printf '  */repos/%s/releases/latest) %s ;;\n' \
    "$repo" "$(curl_stub_answer "$version" same)" >>"$scan_dir/curl_arms"
done < <(jq -r '.drivers | to_entries[] | [.value.repo, .value.version] | @tsv' "$REAL_DRIVER_LOCK")
write_stubs "$scan_dir"
run_scan "$scan_dir" \
  PROVIDER_LOCK_FILE="$REAL_PROVIDER_LOCK" \
  DRIVER_LOCK_FILE="$REAL_DRIVER_LOCK" \
  DRIVER_DOCKERFILE="$REAL_DOCKERFILE" \
  GO_MOD_FILE="$REAL_GO_MOD"

if ((scan_rc != 0)); then
  fail "the scan failed against the committed locks (exit $scan_rc)"
fi
expected_providers=$(jq '.packages | length' "$REAL_PROVIDER_LOCK")
if [[ $(visited_providers) != "$expected_providers" ]]; then
  fail "the scan queried $(visited_providers) of the $expected_providers packages in the committed provider lock"
fi
expected_drivers=$(jq '.drivers | length' "$REAL_DRIVER_LOCK")
if [[ $(visited_drivers) != "$expected_drivers" ]]; then
  fail "the scan queried $(visited_drivers) of the $expected_drivers drivers in the committed driver lock"
fi
if [[ $(jq -r '.drivers | keys | sort | join(",")' "$REAL_DRIVER_LOCK") != 'boxlite,microsandbox' ]]; then
  fail "the committed driver lock must pin exactly boxlite and microsandbox"
fi
for unchecked in docker k8s; do
  if jq -e --arg driver "$unchecked" '.drivers[$driver] != null' "$REAL_DRIVER_LOCK" >/dev/null; then
    fail "the committed driver lock must not track $unchecked upstream"
  fi
done
if ! grep -qF '所有固定的 provider 与 driver 版本都与上游最新版本一致，无需处理。' <(notify); then
  fail "a current set of committed locks did not produce an up-to-date summary: $(notify)"
fi
if grep -q '^- ' <(notify); then
  fail "a current set of committed locks produced upgrade bullets: $(notify)"
fi
if [[ -n $(bump_args) || $(status) != 'false' ]]; then
  fail "the committed locks are not scannable as current: bump=($(bump_args)) outdated='$(status)'"
fi

# 15. PROVIDER_UPGRADE_MENTIONS names the people who should read the summary, and
#     the push is the copy they actually read, so it must reach the summary.
scan mentions <<'EOF'
provider|opencode|opencode-ai|1.18.9|1.19.0|
driver|boxlite|boxlite-ai/boxlite|v0.10.4|same|
EOF
run_scan "$scan_dir" PROVIDER_UPGRADE_MENTIONS='<!-- @alice -->'

if ! grep -qF '<!-- @alice -->' <(notify); then
  fail 'PROVIDER_UPGRADE_MENTIONS did not reach the pushed summary'
fi

if ((failures > 0)); then
  printf 'test-check-upstreams: %d contract check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'test-check-upstreams: all checks passed\n'
