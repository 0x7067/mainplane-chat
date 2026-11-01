#!/usr/bin/env bash
# say.sh <channel or thread id> <text>: post text as the tester bot, the way a
# person types it; prints the new message's id. The bot handles it only while
# its MAINPLANE_DISCORD_TESTER is the tester's user id.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-discord/tester.env"; set +a
jq -n --arg c "$2" '{content: $c}' |
  curl -fsS -X POST -H "Authorization: Bot $TESTER_TOKEN" -H "Content-Type: application/json" \
    --data @- "https://discord.com/api/v10/channels/$1/messages" | jq -r .id
