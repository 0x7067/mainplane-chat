#!/usr/bin/env bash
# slack-say.sh <text> [thread ts]: post text in the user's DM with the app (or
# in $SLACK_CH, when set), as the user, through slack-axi (with the user's
# approval); prints the message's ts. slack-axi's messages carry its own bot_id, and the bot takes them as the
# user's.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-chat/tester.env"; set +a
d=$(slack-axi draft "${SLACK_CH:-$SLACK_DM}" "$1" ${2:+--reply "$2"} </dev/null | awk '/^draft:/ {print $2}')
ts=$(slack-axi draft send "$d" </dev/null | awk -F'"' '/^ts:/ {print $2}')
ts=${ts/./} # slack-axi prints it without its dot
echo "${ts:0:10}.${ts:10}"
