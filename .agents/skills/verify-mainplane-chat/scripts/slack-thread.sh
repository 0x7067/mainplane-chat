#!/usr/bin/env bash
# slack-thread.sh <thread ts>: the DM thread as the bot sees it, oldest first:
# ts, author, reactions, text.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-chat/env"; . "$HOME/.config/mainplane-chat/tester.env"; set +a
curl -fsS -H "Authorization: Bearer $SLACK_BOT_TOKEN" "https://slack.com/api/conversations.replies?channel=$SLACK_DM&ts=$1&limit=200" |
  jq -er 'if .ok then . else error(.error) end | .messages[] | "\(.ts) \(.user // .bot_id) [\((.reactions // []) | map(.name) | join(","))] \(.text | gsub("\n"; " ⏎ "))"'
