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
users=$(tr , '\n' <<<"${MAINPLANE_SLACK_USERS:-}" | tr -d ' \t') # split and trimmed, as the bot does
grep -qx '\*' <<<"$users" && all=yes || all=no
echo "users     $(grep -cv '^\*\{0,1\}$' <<<"$users") listed, every full member: $all"
echo "dm        ${SLACK_DM:-unset}"
echo "channel   ${SLACK_CH:-unset}"
r() { printf '%-10s %s\n' "$1" "$2"; }
if [ -z "${SLACK_DM:-}" ]; then r dm-recipes "skip: SLACK_DM unset in tester.env"
elif lacks im:history chat:write; then r dm-recipes "skip: no im:history or chat:write, DMs never arrive or get no reply"; else
  d="run: Start, Framing, Turn status, Commands, Quote, Catch-up, Gone, Stop"
  lacks reactions:write && d+="; no reactions:write, no 👀, ✅ or ❌"
  r dm-recipes "$d"
  lacks assistant:write && r agent "degraded: no assistant:write, no agent session title, working state or Stop; the log shows 'session status' errors" || r agent "run"
  f="run: names and places"
  lacks users:read && f="degraded: no users:read, names are user ids, [<id> <@<id>>]"
  lacks im:read && f+="; no im:read, the note says 'a Slack conversation'"
  r framing "$f"
fi
if [ -z "${SLACK_CH:-}" ]; then r channels "skip: SLACK_CH unset"; else
  # Its type picks the scopes; without the read scope to look it up, it needs every history scope.
  kind=$(curl -fsS -H "Authorization: Bearer $SLACK_BOT_TOKEN" "https://slack.com/api/conversations.info?channel=$SLACK_CH" |
    jq -r 'if .ok then (if .channel.is_mpim then "mpim" elif .channel.is_private then "groups" else "channels" end) else "" end')
  hist=${kind:+$kind:history}; hist=${hist:-channels:history groups:history mpim:history}
  if lacks $hist; then r channels "skip: no$(for s in $hist; do lacks $s && printf ' %s' $s; done), its messages never arrive"
  elif [ -z "$kind" ]; then r channels "degraded: cannot look up $SLACK_CH (no read scope, or the app is not in it), the note says 'a Slack conversation'"
  else r channels "run"; fi
fi
if [ $all = no ]; then r everyone "skip: no * in MAINPLANE_SLACK_USERS"
elif lacks users:read; then r everyone "degraded: no users:read, only listed users are heard; others log 'users.info <id>: missing_scope'"
else r everyone "code-only: TestFullMember"; fi
