#!/usr/bin/env bash
#
# Contract checks for scripts/check-provider-upstreams.sh.
#
# The daily scan is the only thing that reports an upstream provider release, so
# its silent-failure modes are the behavior worth pinning down.
#
# Every case runs against its own fixture lock and a stubbed `npm`, so the cases
# neither touch the network nor depend on the versions currently pinned in
# guest-images/providers.lock.json. A provider bump is the routine this check
# exists to support; it must not be able to fail the check.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
CHECK="$ROOT_DIR/scripts/check-provider-upstreams.sh"
REAL_LOCK="$ROOT_DIR/guest-images/providers.lock.json"

failures=0
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT

fail() {
  printf 'test-check-provider-upstreams: %s\n' "$*" >&2
  failures=$((failures + 1))
}

report() { cat "$scan_dir/out/provider-upstream-report.md" 2>/dev/null || true; }
bump_args() { cat "$scan_dir/out/provider-upstream-bump.txt" 2>/dev/null || true; }
status() { cat "$scan_dir/out/provider-upstream-outdated" 2>/dev/null || true; }
row() { grep -F "| \`$1\` |" <(report) || true; }

# write_stub installs a fake npm that answers "view <package> version" from
# "<package>) ... ;;" case arms read on stdin.
write_stub() { # $1=case directory
  local dir=$1
  mkdir -p "$dir/bin"
  {
    echo '#!/usr/bin/env bash'
    echo 'case "$2" in'
    cat
    echo '  *) echo "" ;;'
    echo 'esac'
  } >"$dir/bin/npm"
  chmod +x "$dir/bin/npm"
}

# run_scan runs the check against a lock and records the outcome in scan_rc.
run_scan() { # $1=case directory, $2=lock file
  mkdir -p "$1/out"
  set +e
  PATH="$1/bin:$PATH" \
    PROVIDER_LOCK_FILE="$2" \
    PROVIDER_UPSTREAM_OUTPUT_DIR="$1/out" \
    GITHUB_OUTPUT="$1/github_output" \
    "$CHECK" >"$1/stdout" 2>&1
  scan_rc=$?
  set -e
}

# scan builds a fixture lock and a stub npm for one case, then runs the check.
# The spec arrives on stdin as "<kind>|<npm package>|<pinned>|<stub>|<retiredBy>"
# lines, in the order the lock should list them, where
#   <stub>       is "same" to answer with the pinned version, "!fail" to make the
#                query exit non-zero (a delisted, renamed, or rate-limited
#                package), or a version to report instead;
#   <retiredBy>  is empty, or "<package>@<version>" for lock.retiredBy.
#
# It sets scan_dir (the case directory holding the outputs) and scan_rc.
scan() { # $1=case name, spec on stdin
  local kind package pinned stub retired
  scan_dir="$TEST_ROOT/$1"
  mkdir -p "$scan_dir"

  local lock_entries='' stub_arms=''
  while IFS='|' read -r kind package pinned stub retired; do
    [[ -n $kind ]] || continue
    local retired_by=null
    if [[ -n $retired ]]; then
      retired_by=$(printf '{"package":"%s","version":"%s"}' "${retired%@*}" "${retired#*@}")
    fi
    lock_entries+=$(printf '"%s":{"npm":"%s","version":"%s","bin":"%s","retiredBy":%s},' \
      "$kind" "$package" "$pinned" "$kind" "$retired_by")
    if [[ $stub == '!fail' ]]; then
      stub_arms+=$(printf '  %s) exit 1 ;;\n' "$package")
    elif [[ $stub == 'same' ]]; then
      stub_arms+=$(printf '  %s) echo %s ;;\n' "$package" "$pinned")
    else
      stub_arms+=$(printf '  %s) echo %s ;;\n' "$package" "$stub")
    fi
  done

  printf '{"guestAbi":1,"nodeMajor":22,"packages":{%s}}\n' "${lock_entries%,}" >"$scan_dir/lock.json"
  # A malformed fixture would make every assertion below vacuous.
  jq -e . "$scan_dir/lock.json" >/dev/null

  printf '%s' "$stub_arms" | write_stub "$scan_dir"
  run_scan "$scan_dir" "$scan_dir/lock.json"
}

# 1. One failing npm query must not end the scan: the scan reports the failure
#    and keeps going, because a single delisted package would otherwise hide
#    every other provider's release. The failing package is listed first, so the
#    later rows are the proof that the loop survived it.
scan failing-query <<'EOF'
codex|@openai/codex|0.146.0|!fail|
claude|@anthropic-ai/claude-code|2.1.220|same|
dsh|@deepseek-ai/dsh|0.3.0-rc.1|same|
EOF

if ((scan_rc != 0)); then
  fail "a failing npm query aborted the scan (exit $scan_rc)"
fi
if ! grep -q 'unknown (npm query failed)' <(row codex); then
  fail 'a failing npm query was not reported as unknown'
fi
if ! grep -q 'up to date' <(row claude) || ! grep -q 'up to date' <(row dsh); then
  fail 'the scan stopped at the failing query instead of reporting the remaining providers'
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

# 2. A final release must be reported while a prerelease of it is pinned. GNU
#    version sort orders 0.2.0-rc.1 after 0.2.0, so this is exactly the upgrade
#    the plain comparison used to hide as "pinned ahead".
scan prerelease-pin <<'EOF'
claude|@anthropic-ai/claude-code|2.1.220|same|
dsh|@deepseek-ai/dsh|0.2.0-rc.1|0.2.0|
EOF

if ((scan_rc != 0)); then
  fail "the scan failed for a pinned prerelease (exit $scan_rc)"
fi
if ! grep -q '\*\*outdated\*\*' <(row dsh); then
  fail 'a final release of a pinned prerelease was not reported as outdated'
fi
if ! grep -q 'dsh=0.2.0' <(bump_args); then
  fail "the final release was not offered as a bump: $(bump_args)"
fi
if [[ $(status) != 'true' ]]; then
  fail "an available release reported outdated='$(status)'"
fi
if ! grep -q '^outdated=true$' "$scan_dir/github_output"; then
  fail 'the GITHUB_OUTPUT status was not written for an available release'
fi

# 3. A prerelease upstream of the release we already pin is not an upgrade, so it
#    must stay out of the bump arguments. This is the other half of case 2.
scan prerelease-upstream <<'EOF'
claude|@anthropic-ai/claude-code|2.1.220|same|
dsh|@deepseek-ai/dsh|0.2.0|0.2.0-rc.1|
EOF

if ! grep -q 'pinned ahead of npm latest' <(row dsh); then
  fail 'a prerelease upstream of the pinned release was not reported as pinned ahead'
fi
if [[ -n $(bump_args) ]]; then
  fail "a prerelease upstream produced bump arguments: $(bump_args)"
fi
if [[ $(status) != 'false' ]]; then
  fail "a prerelease upstream reported outdated='$(status)'"
fi

# 4. A retired companion is reported so someone can act, but it is never offered
#    as a bump: the recorded plan is to delete it in the upgrade that retires it.
scan retired-companion <<'EOF'
opencode|opencode-ai|1.18.9|1.19.0|
pi|@earendil-works/pi-coding-agent|0.82.1|same|
pi-mcp-adapter|pi-mcp-adapter|2.11.0|5.3.0|pi@0.99.0
EOF

if ! grep -q 'retired by `pi 0.99.0`' <(row pi-mcp-adapter); then
  fail 'a retired companion was not annotated with the release that retires it'
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

# 5. The committed lock must stay scannable: every pinned package is queried and
#    reported. The stub echoes each package's own pin, read from the lock, so a
#    provider bump keeps this case passing while a lock the scan cannot read
#    still fails it.
scan_dir="$TEST_ROOT/committed-lock"
mkdir -p "$scan_dir"
jq -r '.packages | to_entries[] | "  \(.value.npm)) echo \(.value.version) ;;"' "$REAL_LOCK" |
  write_stub "$scan_dir"
run_scan "$scan_dir" "$REAL_LOCK"

if ((scan_rc != 0)); then
  fail "the scan failed against the committed lock (exit $scan_rc)"
fi
expected_rows=$(jq '.packages | length' "$REAL_LOCK")
actual_rows=$(grep -c '^| `' <(report) || true)
if [[ $actual_rows != "$expected_rows" ]]; then
  fail "the scan reported $actual_rows of the $expected_rows packages in the committed lock"
fi
if [[ -n $(bump_args) || $(status) != 'false' ]]; then
  fail "the committed lock is not scannable as current: bump=($(bump_args)) outdated='$(status)'"
fi

if ((failures > 0)); then
  printf 'test-check-provider-upstreams: %d contract check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'test-check-provider-upstreams: all checks passed\n'
