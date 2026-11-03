#!/usr/bin/env bash
# doctor.sh [rev]: read-only health check of the running bot. The binary must be
# built from rev, by default the HEAD of the checkout it runs in.
# Linux runs the systemd user service; macOS runs the launchd LaunchAgent local.mainplane-chat.
set -uo pipefail
ok=1
say() { printf '%-8s %s\n' "$1" "$2"; if [ "$1" = FAIL ]; then ok=0; fi; }
if [ "$(uname -s)" = Darwin ]; then
	bin="$HOME/.local/bin/mainplane-chat"
	agent="gui/$(id -u)/local.mainplane-chat"
	launchctl print "$agent" 2>/dev/null | grep -q 'state = running' && say ok "service running" || say FAIL "service not running"
	pid="$(launchctl print "$agent" 2>/dev/null | sed -n 's/^[[:space:]]*pid = //p')"
	since=""
	[ -n "$pid" ] && since="$(date -j -f '%a %b %e %T %Y' "$(ps -o lstart= -p "$pid")" '+%Y/%m/%d %H:%M:%S' 2>/dev/null)"
	# The log lines start with a yyyy/mm/dd hh:mm:ss stamp, so comparing the first 19 bytes orders them by time.
	[ -n "$since" ] && awk -v s="$since" 'substr($0, 1, 19) >= s' "$HOME/Library/Logs/mainplane-chat.log" | grep -q 'caught up' && say ok "caught up since $since" || say FAIL "no 'caught up' since ${since:-the service started}"
else
	bin="$HOME/go/bin/mainplane-chat"
	systemctl --user is-active -q mainplane-chat && say ok "service active" || say FAIL "service not active"
	since="$(systemctl --user show -p ActiveEnterTimestamp --value mainplane-chat)"
	journalctl --user -u mainplane-chat --since "$since" -o cat --no-pager | grep -q 'caught up' && say ok "caught up since $since" || say FAIL "no 'caught up' since $since"
fi
rev="$(go version -m "$bin" 2>/dev/null | sed -n 's/.*vcs.revision=//p')"
head="${1:-$(git rev-parse HEAD 2>/dev/null)}"
dirty="$(go version -m "$bin" 2>/dev/null | sed -n 's/.*vcs.modified=//p')"
[ -n "$rev" ] && [ "$rev" = "$head" ] && say ok "binary built from HEAD ${head:0:7}${dirty:+, modified=$dirty}" || say FAIL "binary rev ${rev:-unknown} is not HEAD ${head:0:7}"
code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/)"
[ "$code" != 000 ] && say ok "harness answers (HTTP $code)" || say FAIL "harness at 127.0.0.1:8080 does not answer"
jq -e . "$HOME/.local/state/mainplane-chat/state.json" >/dev/null && say ok "state file parses" || say FAIL "state file missing or bad"
[ $ok = 1 ]
