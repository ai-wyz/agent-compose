#!/usr/bin/env bash
#
# Contract checks for scripts/check-provider-upstreams.sh.
#
# The daily scan is the only thing that reports an upstream provider release, so
# its silent-failure modes are the behavior worth pinning down. Each case stubs
# the `npm` executable, so the script is exercised without touching the network.
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
CHECK="$ROOT_DIR/scripts/check-provider-upstreams.sh"

failures=0
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT

fail() {
  printf 'test-check-provider-upstreams: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# scan runs the check once with a stubbed npm. The spec arrives on stdin as
# "<package>|<version>" lines; a version of "!fail" makes the query exit non-zero,
# which is how a delisted, renamed, or rate-limited package behaves.
#
# It sets scan_dir (the case directory holding the outputs) and scan_rc.
scan() { # $1=case name
  local name=$1
  scan_dir="$TEST_ROOT/$name"
  mkdir -p "$scan_dir/bin" "$scan_dir/out"

  {
    echo '#!/usr/bin/env bash'
    echo 'case "$2" in'
    while IFS='|' read -r package version; do
      [[ -n $package ]] || continue
      if [[ $version == '!fail' ]]; then
        printf '  %s) exit 1 ;;\n' "$package"
      else
        printf '  %s) echo %s ;;\n' "$package" "$version"
      fi
    done
    echo '  *) echo "" ;;'
    echo 'esac'
  } >"$scan_dir/bin/npm"
  chmod +x "$scan_dir/bin/npm"

  set +e
  PATH="$scan_dir/bin:$PATH" \
    PROVIDER_UPSTREAM_OUTPUT_DIR="$scan_dir/out" \
    GITHUB_OUTPUT="$scan_dir/github_output" \
    "$CHECK" >"$scan_dir/stdout" 2>&1
  scan_rc=$?
  set -e
}

report() { cat "$scan_dir/out/provider-upstream-report.md"; }
bump_args() { cat "$scan_dir/out/provider-upstream-bump.txt" 2>/dev/null || true; }
status() { cat "$scan_dir/out/provider-upstream-outdated" 2>/dev/null || true; }

# 1. One failing npm query must not end the scan. The scan reports the failure
#    and keeps going, because a single delisted package would otherwise hide
#    every other provider's release.
scan failing-query <<'EOF'
@openai/codex|!fail
@anthropic-ai/claude-code|2.1.220
opencode-ai|1.18.9
@earendil-works/pi-coding-agent|0.82.1
pi-mcp-adapter|2.11.0
@deepseek-ai/dsh|0.2.0-rc.1
EOF

if ((scan_rc != 0)); then
  fail "a failing npm query aborted the scan (exit $scan_rc)"
fi
if ! grep -q 'unknown (npm query failed)' <(report); then
  fail 'a failing npm query was not reported as unknown'
fi
# The providers after the failure are the proof that the loop survived it.
if ! grep -q '`claude`.*up to date' <(report) || ! grep -q '`dsh`.*up to date' <(report); then
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
@openai/codex|0.146.0
@anthropic-ai/claude-code|2.1.220
opencode-ai|1.18.9
@earendil-works/pi-coding-agent|0.82.1
pi-mcp-adapter|2.11.0
@deepseek-ai/dsh|0.2.0
EOF

if ((scan_rc != 0)); then
  fail "the scan failed for a pinned prerelease (exit $scan_rc)"
fi
if ! grep -q '`dsh`.*\*\*outdated\*\*' <(report); then
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
@openai/codex|0.146.0
@anthropic-ai/claude-code|2.1.220
opencode-ai|1.18.9
@earendil-works/pi-coding-agent|0.82.1-rc.2
pi-mcp-adapter|2.11.0
@deepseek-ai/dsh|0.2.0-rc.1
EOF

if ! grep -q '`pi`.*pinned ahead of npm latest' <(report); then
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
@openai/codex|0.146.0
@anthropic-ai/claude-code|2.1.220
opencode-ai|1.19.0
@earendil-works/pi-coding-agent|0.82.1
pi-mcp-adapter|5.3.0
@deepseek-ai/dsh|0.2.0-rc.1
EOF

if ! grep -q 'retired by `pi 0.99.0`' <(report); then
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

if ((failures > 0)); then
  printf 'test-check-provider-upstreams: %d contract check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'test-check-provider-upstreams: all checks passed\n'
