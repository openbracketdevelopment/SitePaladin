#!/bin/sh
set -eu
: "${API_KEY:?Set API_KEY server-side}"
: "${1:?Usage: sh examples/curl.sh image.png}"
base="${BYTEDOWN_URL:-http://127.0.0.1:8080}"
scheme='Bearer'
curl --fail-with-body --silent --show-error --max-time 150 \
  -H "Authorization: $scheme $API_KEY" \
  -F "file=@$1" -F 'profile=web' "$base/v1/images"
printf '\nUse the returned status_url with GET until succeeded or failed.\n'
