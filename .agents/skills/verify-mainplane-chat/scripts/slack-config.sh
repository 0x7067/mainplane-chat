#!/usr/bin/env bash
# slack-config.sh: read-only, the deployment's Slack setup and which Slack
# recipes (features/slack.md) run, and how: the app, the manifest scopes its
# token lacks, who it hears, and the tester's DM and channel.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-chat/env"; [ -f "$HOME/.config/mainplane-chat/tester.env" ] && . "$HOME/.config/mainplane-chat/tester.env"; set +a
if [ -z "${SLACK_BOT_TOKEN:-}" ]; then echo "slack     off: SLACK_BOT_TOKEN unset, skip every Slack recipe"; exit 0; fi
manifest="$(dirname "$0")/../../../../slack-manifest.yaml"
hdr=$(curl -fsS -D - -o /tmp/slack-config.$$ -H "Authorization: Bearer $SLACK_BOT_TOKEN" https://slack.com/api/auth.test)
jq -r '"app       \(.user) \(.user_id), team \(.team)"' /tmp/slack-config.$$; rm /tmp/slack-config.$$
have=" $(printf '%s' "$hdr" | sed -n 's/^x-oauth-scopes: *//Ip' | tr -d '\r' | tr , ' ') "
missing=""; for s in $(sed -n '/^    bot:/,/^settings:/s/^ *- //p' "$manifest"); do [[ $have == *" $s "* ]] || missing+=" $s"; done
lacks() { for s; do [[ $missing == *" $s"* ]] && return 0; done; return 1; }
echo "missing  ${missing:- none}"
users=${MAINPLANE_SLACK_USERS:-}; [[ ",$users," == *",*,"* ]] && all=yes || all=no
echo "users     $(tr , '\n' <<<"$users" | grep -cv '^\*\?$') listed, every full member: $all"
echo "dm        ${SLACK_DM:-unset}"
echo "channel   ${SLACK_CH:-unset}"
r() { printf '%-10s %s\n' "$1" "$2"; }
if [ -z "${SLACK_DM:-}" ]; then r dm-recipes "skip: SLACK_DM unset in tester.env"; else
  lacks im:history && r dm-recipes "skip: no im:history, DMs never arrive" || r dm-recipes "run: Start, Turn status, Commands, Quote, Catch-up, Gone, Stop"
  lacks assistant:write && r agent "degraded: no assistant:write, no agent session title, working state or Stop; the log shows 'session status' errors" || r agent "run"
  f="run: names and places"
  lacks users:read && f="degraded: no users:read, names are user ids, [<id> <@<id>>]"
  lacks im:read && f+="; no im:read, the note says 'a Slack conversation'"
  r framing "$f"
fi
if [ -z "${SLACK_CH:-}" ]; then r channels "skip: SLACK_CH unset"
elif lacks channels:history groups:history mpim:history; then r channels "skip: no$(for s in channels:history groups:history mpim:history; do lacks $s && printf ' %s' $s; done), channel messages never arrive"
else r channels "run"; fi
if [ $all = no ]; then r everyone "skip: no * in MAINPLANE_SLACK_USERS"
elif lacks users:read; then r everyone "degraded: no users:read, only listed users are heard; others log 'users.info <id>: missing_scope'"
else r everyone "code-only: TestFullMember"; fi
