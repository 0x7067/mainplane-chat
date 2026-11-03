#!/usr/bin/env bash
# doctor.sh: read-only health check of the bot this host runs, from a checkout of the repo.
# The host describes its deployment in ~/.config/mainplane-chat/deploy.sh (SKILL.md, "Deployment").
set -uo pipefail
deploy="$HOME/.config/mainplane-chat/deploy.sh"
[ -f "$deploy" ] || { echo "FAIL     no $deploy: describe this host's deployment first (SKILL.md, Deployment)"; exit 1; }
. "$deploy"
ok=1
say() { printf '%-8s %s\n' "$1" "$2"; if [ "$1" = FAIL ]; then ok=0; fi; }
mpc_running && say ok "service running" || say FAIL "service not running"
mpc_log_since_start | grep -q 'caught up' && say ok "caught up since it started" || say FAIL "no 'caught up' since it started"
rev="$(go version -m "$MPC_BIN" 2>/dev/null | sed -n 's/.*vcs.revision=//p')"
head="$(git rev-parse HEAD 2>/dev/null)"
dirty="$(go version -m "$MPC_BIN" 2>/dev/null | sed -n 's/.*vcs.modified=//p')"
[ -n "$rev" ] && [ "$rev" = "$head" ] && say ok "binary built from HEAD ${head:0:7}${dirty:+, modified=$dirty}" || say FAIL "binary rev ${rev:-unknown} is not HEAD ${head:0:7}"
harness="${MAINPLANE_URL:-http://127.0.0.1:8080}"
code="$(curl -s -o /dev/null -w '%{http_code}' "$harness/")"
[ "$code" != 000 ] && say ok "harness answers (HTTP $code)" || say FAIL "harness at $harness does not answer"
state="${XDG_STATE_HOME:-$HOME/.local/state}/mainplane-chat/state.json"
jq -e . "$state" >/dev/null && say ok "state file parses" || say FAIL "state file missing or bad"
[ $ok = 1 ]
