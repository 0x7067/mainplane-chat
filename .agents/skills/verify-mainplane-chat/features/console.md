# Console

`mainplane-chat -console` runs the bridge in the terminal against the harness: no chat tokens, a throwaway state file, the running bot untouched. Each stdin line is a message in one thread. It is the first check of a build, before Launch (README, "Trying a build").

## Sub-features

- `console-new` the first line starts a session (`via=console`); later lines are messages or thread commands.
- `console-session` `-session <id>` shows an existing session instead.
- `console-print` it prints `say:` replies, `react` marks (👀, then ✅ or ❌) on the message and the thread, and `status:` lines titled as in Slack.
- `console-wait` at the end of input it waits up to `-wait` (default 10m) for the session to stop working.
- `console-unmirrored` a running bot does not mirror a console session in `#sessions`.

## Driving it

Build to a temp path, never `$MPC_BIN`, and load the env: `go build -o /tmp/mpc-console . && set -a && . ~/.config/mainplane-chat/env && set +a`.

- **Existing.** `printf '!info\n' | /tmp/mpc-console -console -session <id> -wait 5s` prints `say: session \`<id>\` <status>`, the model, workers and context; for a working session, then `console: session <id> still working after 5s`.
- **New.** `printf 'verify console <run>: reply with just the word ok\n' | /tmp/mpc-console -console -wait 3m` prints `react <message>: 👀`, `react thread: 👀`, both again with ✅, then `say: ok`. The newest in `mainplane sessions` is it; `mainplane tail <id>` shows `via=console`. With a bot at this build running, the state file has no thread for it.
- **Status.** `... use the run tool on <worker> to execute: echo hi, then reply with just the word done` prints `status: Working · 1 call` with `in_progress run on <worker>`, a `rename:` line, then `complete`, then `status: Done · 1 call · Ts · P% context · <id>`, the marks, and `say: done`.

## Gotchas

- The session id shows only in the `Done` status line of a turn with calls; otherwise find it with `mainplane sessions`. Delete it in cleanup.
