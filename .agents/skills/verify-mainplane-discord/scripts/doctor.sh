#!/usr/bin/env bash
# doctor.sh: read-only health check of the running bot, from a checkout of the repo.
set -uo pipefail
ok=1
say() { printf '%-8s %s\n' "$1" "$2"; if [ "$1" = FAIL ]; then ok=0; fi; }
systemctl --user is-active -q mainplane-discord && say ok "service active" || say FAIL "service not active"
bin="$HOME/go/bin/mainplane-chat"
rev="$(go version -m "$bin" 2>/dev/null | sed -n 's/.*vcs.revision=//p')"
head="$(git rev-parse HEAD 2>/dev/null)"
dirty="$(go version -m "$bin" 2>/dev/null | sed -n 's/.*vcs.modified=//p')"
[ -n "$rev" ] && [ "$rev" = "$head" ] && say ok "binary built from HEAD ${head:0:7}${dirty:+, modified=$dirty}" || say FAIL "binary rev ${rev:-unknown} is not HEAD ${head:0:7}"
since="$(systemctl --user show -p ActiveEnterTimestamp --value mainplane-discord)"
journalctl --user -u mainplane-discord --since "$since" -o cat --no-pager | grep -q 'caught up' && say ok "caught up since $since" || say FAIL "no 'caught up' since $since"
code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/)"
[ "$code" != 000 ] && say ok "harness answers (HTTP $code)" || say FAIL "harness at 127.0.0.1:8080 does not answer"
jq -e . "$HOME/.local/state/mainplane-chat/state.json" >/dev/null && say ok "state file parses" || say FAIL "state file missing or bad"
[ $ok = 1 ]
