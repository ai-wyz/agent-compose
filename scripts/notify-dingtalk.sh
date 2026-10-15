#!/usr/bin/env bash
#
# Send one Markdown message to every DingTalk target in DINGTALK_TARGETS_JSON.
#
# The release workflow and the scheduled provider-upstream check need the same
# signing, masking, and multi-target retry behaviour, so it lives here instead of
# being duplicated in two workflow files.
#
# Usage: notify-dingtalk.sh <title>   (message body on stdin)
#
# An unset or empty DINGTALK_TARGETS_JSON is deliberately not an error: a
# repository may track upstream releases through the GitHub issue alone, and
# failing a scheduled job because no webhook is configured would only add noise.
# A configured target that rejects the message IS an error.
set -euo pipefail

title=${1:-}
if [[ -z $title ]]; then
  echo 'notify-dingtalk: a message title is required' >&2
  exit 2
fi

targets=${DINGTALK_TARGETS_JSON:-}
if [[ -z $targets ]]; then
  echo 'notify-dingtalk: DINGTALK_TARGETS_JSON is not configured; skipping the push notification'
  exit 0
fi

if ! jq -e '
  type == "array" and
  length > 0 and
  all(.[];
    type == "object" and
    (.webhook | type == "string" and length > 0) and
    (.secret | type == "string" and length > 0)
  )
' <<<"$targets" >/dev/null 2>&1; then
  echo 'notify-dingtalk: DINGTALK_TARGETS_JSON must be a non-empty array of objects with non-empty webhook and secret strings' >&2
  exit 1
fi

text=$(cat)
if [[ -z $text ]]; then
  echo 'notify-dingtalk: refusing to send an empty message' >&2
  exit 2
fi

payload=$(jq -n --arg title "$title" --arg text "$text" \
  '{msgtype: "markdown", markdown: {title: $title, text: $text}}')

target_count=$(jq -r 'length' <<<"$targets")
failed_count=0
for ((index = 0; index < target_count; index++)); do
  target_number=$((index + 1))
  webhook=$(jq -er ".[$index].webhook" <<<"$targets")
  signing_secret=$(jq -er ".[$index].secret" <<<"$targets")
  # Mask the endpoints and the derived signature so a public workflow log cannot
  # leak a webhook that carries its own signing secret in the URL.
  echo "::add-mask::$webhook"
  echo "::add-mask::$signing_secret"

  timestamp="$(date +%s)000"
  string_to_sign="$(printf '%s\n%s' "$timestamp" "$signing_secret")"
  sign="$(printf '%s' "$string_to_sign" \
    | openssl dgst -sha256 -hmac "$signing_secret" -binary \
    | base64 \
    | tr -d '\r\n')"
  encoded_sign="$(jq -rn --arg sign "$sign" '$sign | @uri')"
  echo "::add-mask::$encoded_sign"

  separator='?'
  if [[ $webhook == *\?* ]]; then
    separator='&'
  fi
  signed_webhook="${webhook}${separator}timestamp=${timestamp}&sign=${encoded_sign}"
  echo "::add-mask::$signed_webhook"

  if ! response="$(curl -fsS \
    --retry 3 \
    --connect-timeout 10 \
    --max-time 30 \
    -X POST \
    -H 'Content-Type: application/json' \
    --data "$payload" \
    "$signed_webhook")"; then
    echo "notify-dingtalk: HTTP request failed for target #${target_number}" >&2
    failed_count=$((failed_count + 1))
    continue
  fi

  error_message="$(jq -r '
    if type != "object" then "invalid response"
    elif (.errcode // 0) == 0 then ""
    else (.errmsg // "unknown error")
    end
  ' <<<"$response" 2>/dev/null || echo 'invalid response')"
  if [[ -n $error_message ]]; then
    echo "notify-dingtalk: target #${target_number} rejected the message: ${error_message}" >&2
    failed_count=$((failed_count + 1))
    continue
  fi

  echo "notify-dingtalk: sent to target #${target_number}"
done

if ((failed_count > 0)); then
  echo "notify-dingtalk: ${failed_count} of ${target_count} targets failed" >&2
  exit 1
fi
