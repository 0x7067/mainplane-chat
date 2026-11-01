#!/usr/bin/env bash
# thread.sh <channel or thread id> [limit]: its newest messages, oldest first,
# as "time author: text", the first line of each.
set -euo pipefail
here="$(dirname "$0")"
"$here/discord.sh" "/channels/$1/messages?limit=${2:-20}" |
  jq -r 'reverse[] | "\(.timestamp[11:19]) \(.author.username)\(if .thread then " [thread \(.thread.id)]" else "" end): \(.content | split("\n")[0])"'
