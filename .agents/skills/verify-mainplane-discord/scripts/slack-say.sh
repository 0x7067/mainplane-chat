#!/usr/bin/env bash
# slack-say.sh <text> [thread ts]: post text in the user's DM with the app, as
# the user, through slack-axi (he approved it for tests); prints the message's
# ts. slack-axi's messages carry its own bot_id, and the bot takes them as the
# user's.
set -euo pipefail
d=$(slack-axi draft D0CE46P2QJY "$1" ${2:+--reply "$2"} </dev/null | awk '/^draft:/ {print $2}')
ts=$(slack-axi draft send "$d" </dev/null | awk -F'"' '/^ts:/ {print $2}')
echo "${ts:0:10}.${ts:10}"
