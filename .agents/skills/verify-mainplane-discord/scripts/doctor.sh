#!/usr/bin/env bash
# doctor.sh: read-only health check of the running bot, from a checkout of the repo.
set -uo pipefail
ok=1
say() { printf '%-8s %s\n' "$1" "$2"; [ "$1" = FAIL ] && ok=0; }
systemctl --user is-active -q mainplane-discord && say ok "service active" || say FAIL "service not active"
bin="$HOME/go/bin/mainplane-discord"
rev="$(go version -m "$bin" 2>/dev/null | awk '$2=="vcs.revision"{print $3}')"
head="$(git rev-parse HEAD 2>/dev/null)"
dirty="$(go version -m "$bin" 2>/dev/null | awk '$2=="vcs.modified"{print $3}')"
[ -n "$rev" ] && [ "$rev" = "$head" ] && say ok "binary built from HEAD ${head:0:7}${dirty:+ (modified=$dirty)}" || say FAIL "binary rev ${rev:-unknown} is not HEAD ${head:0:7}"
since="$(systemctl --user show -p ActiveEnterTimestamp --value mainplane-discord)"
journalctl --user -u mainplane-discord --since "$since" -o cat --no-pager | grep -q 'caught up' && say ok "caught up since $since" || say FAIL "no 'caught up' since $since"
curl -fsS -o /dev/null http://127.0.0.1:8080/ && say ok "harness answers" || say FAIL "harness at 127.0.0.1:8080 does not answer"
jq -e . "$HOME/.local/state/mainplane-discord/state.json" >/dev/null && say ok "state file parses" || say FAIL "state file missing or bad"
[ $ok = 1 ]
