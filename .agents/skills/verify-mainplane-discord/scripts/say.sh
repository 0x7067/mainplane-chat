#!/usr/bin/env bash
# say.sh <channel or thread id> <text> [file...]: post text, and files as
# attachments, as the tester bot, the way a person types it; prints the new
# message's id. The bot handles it only while its MAINPLANE_DISCORD_TESTER is
# the tester's user id.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-discord/tester.env"; set +a
ch=$1 text=$2; shift 2
files=(); i=0
for f in "$@"; do files+=(-F "files[$i]=@$f"); i=$((i + 1)); done
curl -fsS -X POST -H "Authorization: Bot $TESTER_TOKEN" \
  --form-string "payload_json=$(jq -n --arg c "$text" '{content: $c}')" "${files[@]}" \
  "https://discord.com/api/v10/channels/$ch/messages" | jq -r .id
