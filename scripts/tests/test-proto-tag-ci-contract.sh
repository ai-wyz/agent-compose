#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
NEXT_TAG="$ROOT_DIR/scripts/proto-next-tag.sh"
REQUIRE_BUMP="$ROOT_DIR/scripts/proto-require-bump.sh"
TAG_WORKFLOW="$ROOT_DIR/.github/workflows/proto-tag.yml"
CI_WORKFLOW="$ROOT_DIR/.github/workflows/ci.yml"
TEST_ROOT=$(mktemp -d)
trap 'rm -rf -- "$TEST_ROOT"' EXIT

failures=0

fail() {
  printf 'test-proto-tag-ci-contract: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# --- proto-next-tag.sh against a scratch repository -------------------------

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com

repo="$TEST_ROOT/repo"
git init -q -b main "$repo"

commit_file() { # $1=path $2=content; prints the new commit
  mkdir -p "$(dirname -- "$repo/$1")"
  printf '%s\n' "$2" >"$repo/$1"
  git -C "$repo" add -- "$1"
  git -C "$repo" commit -q -m "change $1"
  git -C "$repo" rev-parse HEAD
}

expect() { # $1=description $2=commit $3=expected-status $4=expected-stdout
  local out status
  set +e
  out=$(cd "$repo" && "$NEXT_TAG" "$2" 2>/dev/null)
  status=$?
  set -e
  if [[ $status -ne $3 || $out != "$4" ]]; then
    fail "$1: got status $status output '$out', expected status $3 output '$4'"
  fi
}

base=$(commit_file proto/api.proto 'v1')
expect 'no proto tag at all' "$base" 1 ''

git -C "$repo" tag proto/v0.1.2 "$base"
git -C "$repo" tag proto/v0.1.9 "$base"
expect 'tagged commit' "$base" 0 ''

docs=$(commit_file README.md 'docs')
expect 'change outside proto/' "$docs" 0 ''

field=$(commit_file proto/api.proto 'v2')
expect 'proto change after the newest tag' "$field" 0 'proto/v0.1.10'

git -C "$repo" tag proto/v0.1.10 "$field"
rpc=$(commit_file proto/api.proto 'v3')
expect 'numeric rather than lexical version order' "$rpc" 0 'proto/v0.1.11'

git -C "$repo" tag proto/v0.1.11 "$rpc"
git -C "$repo" tag proto/v0.2.0-rc.1 "$rpc"
nested=$(commit_file proto/health/v1/health.proto 'v1')
expect 'pre-release tags ignored' "$nested" 0 'proto/v0.1.12'
expect 'late run for a commit the newest tag contains' "$field" 0 ''

git -C "$repo" checkout -q -b side "$base"
side=$(commit_file proto/api.proto 'side')
expect 'commit that does not descend from the newest tag' "$side" 1 ''
expect 'unknown commit' 'no-such-commit' 1 ''

# --- proto-require-bump.sh against scratch go.mod files -----------------------

write_gomod() { # $1=name, remaining lines on stdin; prints the path
  cat >"$TEST_ROOT/$1"
  printf '%s\n' "$TEST_ROOT/$1"
}

expect_bump() { # $1=description $2=tag $3=go.mod $4=expected-status $5=expected-stdout
  local out status
  set +e
  out=$("$REQUIRE_BUMP" "$2" "$3" 2>/dev/null)
  status=$?
  set -e
  if [[ $status -ne $4 || $out != "$5" ]]; then
    fail "$1: got status $status output '$out', expected status $4 output '$5'"
  fi
}

block=$(write_gomod block.go.mod <<'GOMOD'
module github.com/chaitin/agent-compose

require (
	connectrpc.com/connect v1.19.2
	github.com/chaitin/agent-compose/proto v0.1.9
)

replace github.com/chaitin/agent-compose/proto => ./proto
GOMOD
)
expect_bump 'requirement older than the tag' proto/v0.1.10 "$block" 0 'v0.1.10'
expect_bump 'requirement already at the tag' proto/v0.1.9 "$block" 0 ''
expect_bump 'late run for a tag older than the requirement' proto/v0.1.8 "$block" 0 ''
expect_bump 'pre-release tag' proto/v0.2.0-rc.1 "$block" 1 ''
expect_bump 'tag of another module' v0.1.10 "$block" 1 ''
expect_bump 'missing go.mod' proto/v0.1.10 "$TEST_ROOT/absent.go.mod" 1 ''

single=$(write_gomod single.go.mod <<'GOMOD'
module example.com/client

require github.com/chaitin/agent-compose/proto v0.1.2
GOMOD
)
expect_bump 'single-line requirement' proto/v0.1.10 "$single" 0 'v0.1.10'

unrequired=$(write_gomod unrequired.go.mod <<'GOMOD'
module example.com/client

replace github.com/chaitin/agent-compose/proto => ./proto
GOMOD
)
expect_bump 'replace without a requirement' proto/v0.1.10 "$unrequired" 1 ''

# --- workflow wiring ---------------------------------------------------------

tag_workflow=$(<"$TAG_WORKFLOW")
ci_workflow=$(<"$CI_WORKFLOW")

require() { # $1=text $2=extended-regex $3=description
  if ! grep -Eq -- "$2" <<<"$1"; then
    fail "missing $3"
  fi
}

# workflow_run names the workflow it follows, so renaming CI would silently
# stop every proto release.
require "$ci_workflow" '^name: CI$' 'CI workflow name followed by the proto tag workflow'
require "$tag_workflow" 'workflows: \[CI\]' 'trigger on CI runs'
require "$tag_workflow" 'types: \[completed\]' 'trigger on completed CI runs'
require "$tag_workflow" 'branches: \[main\]' 'main-only trigger'
require "$tag_workflow" "workflow_run\.event == 'push'" 'guard against pull requests from a branch named main'
require "$tag_workflow" "workflow_run\.conclusion == 'success'" 'guard against failed CI runs'
require "$tag_workflow" "github\.ref == 'refs/heads/main'" 'main-only manual dispatch'
require "$tag_workflow" 'cancel-in-progress: false' 'serialized tagging'
require "$tag_workflow" 'contents: write' 'tag push permission'

# Write access belongs to the jobs that use it, not the whole workflow.
top_permissions=$(awk '
  /^permissions:/ { found = 1; next }
  found && /^[^[:space:]]/ { exit }
  found { print }
' "$TAG_WORKFLOW")
require "$top_permissions" '^  contents: read$' 'read-only workflow permissions'
if grep -q 'write' <<<"$top_permissions"; then
  fail 'forbidden write access in workflow-level permissions'
fi
require "$tag_workflow" 'fetch-depth: 0' 'checkout with every tag'
require "$tag_workflow" 'scripts/proto-next-tag\.sh "\$COMMIT"' 'tag decision for the tested commit'
require "$tag_workflow" 'git push origin "refs/tags/\$TAG"' 'tag push'
require "$tag_workflow" 'GOPROXY: https://proxy\.golang\.org$' 'resolution through the public proxy alone'

# Resolution follows the tag the commit carries rather than the tag this run
# decided to push, so a re-run after a failed resolution checks it again.
require "$tag_workflow" 'git tag --points-at "\$COMMIT"' 'lookup of the proto tag on the tested commit'
resolve_step=$(awk '
  /^      - name: Resolve the tag through the module proxy$/ { found = 1; print; next }
  found && /^      - / { exit }
  found { print }
' "$TAG_WORKFLOW")
require "$resolve_step" "if: steps\.published\.outputs\.tag != ''" 'resolution gated on the tag the commit carries'
require "$resolve_step" 'TAG: \$\{\{ steps\.published\.outputs\.tag \}\}' 'resolution of the tag the commit carries'

# The bump follows a tag that this run saw resolve, and only the bump job may
# touch pull requests.
job() { # $1=job id; prints that job's block
  awk -v header="  $1:" '
    $0 == header { found = 1; print; next }
    found && $0 ~ /^  [[:alnum:]_-]+:[[:space:]]*$/ { exit }
    found { print }
  ' "$TAG_WORKFLOW"
}
tag_job=$(job tag)
bump_job=$(job bump)
require "$tag_job" 'published: \$\{\{ steps\.published\.outputs\.tag \}\}' 'tag the commit carries exposed to the bump job'
if grep -q 'pull-requests' <<<"$tag_job"; then
  fail 'forbidden pull request access in the tagging job'
fi
require "$bump_job" '^    needs: tag$' 'bump after the tag resolved'
require "$bump_job" "if: needs\.tag\.outputs\.published != ''" 'bump gated on a published tag'
require "$bump_job" 'pull-requests: write' 'pull request permission for the bump job'
require "$bump_job" 'ref: main$' 'bump based on main'
require "$bump_job" 'scripts/proto-require-bump\.sh "\$TAG" go\.mod' 'bump decision for the root go.mod'
require "$bump_job" 'scripts/proto-require-bump\.sh "\$TAG" "\$RUNNER_TEMP/pending\.go\.mod"' 'bump decision for a pending pull request'
require "$bump_job" 'gh pr create --draft --base main' 'draft pull request against main'

proto_version=$(awk '
  $0 == "  proto-version:" { found = 1; print; next }
  found && $0 ~ /^  [[:alnum:]_-]+:[[:space:]]*$/ { exit }
  found { print }
' "$CI_WORKFLOW")
require "$proto_version" "if: github\.ref_type == 'tag'" 'release-tag-only published proto check'
require "$proto_version" 'go mod edit -dropreplace=github\.com/chaitin/agent-compose/proto' 'build without the proto replace'

if ((failures > 0)); then
  printf 'test-proto-tag-ci-contract: %d check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'test-proto-tag-ci-contract: all checks passed\n'
