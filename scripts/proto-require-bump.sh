#!/usr/bin/env bash
set -euo pipefail

# Decides whether a go.mod should move its proto module requirement to a
# published proto tag. Prints the version to require when the file names an
# older one, and prints nothing when it already names that version or a newer
# one. The Proto tag workflow edits go.mod and opens the pull request; this
# script only decides, so it can be tested without a remote.
#
# Requirements only move forward, for the same reason the tags do: a run for an
# older commit finishing late must not pull the requirement back.

MODULE=github.com/chaitin/agent-compose/proto

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <proto-tag> <go.mod>" >&2
  exit 2
fi

tag=$1
gomod=$2

if [[ ! $tag =~ ^proto/v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "proto-require-bump: not a proto/vX.Y.Z tag: $tag" >&2
  exit 1
fi
version=${tag#proto/}

if [[ ! -f $gomod ]]; then
  echo "proto-require-bump: no such file: $gomod" >&2
  exit 1
fi

# The replace directive names the module too, but carries no version.
required=$(sed -n -E \
  "s|^[[:space:]]*(require[[:space:]]+)?${MODULE//./\\.}[[:space:]]+(v[0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$|\2|p" \
  "$gomod")
if [[ -z $required || $required == *$'\n'* ]]; then
  echo "proto-require-bump: $gomod does not require one $MODULE vX.Y.Z" >&2
  exit 1
fi

if [[ $required == "$version" ]]; then
  echo "proto-require-bump: $gomod already requires $version" >&2
  exit 0
fi

oldest=$(printf '%s\n%s\n' "$required" "$version" | sort -V | head -n 1)
if [[ $oldest != "$required" ]]; then
  echo "proto-require-bump: $gomod requires $required, newer than $version" >&2
  exit 0
fi

printf '%s\n' "$version"
