# Thread commands

In a thread, `/model`, `/continue`, `/retry`, `/stop` and `/info` act on the thread's session as the terminal app and CLI do; `!` works in place of `/`. `/model` and `/continue` continue in a new session, and the thread follows it without being renamed.

## Sub-features

- `cmd-model-list` `/model` shows a select of the connected models (`model:<session id>`, value the preset id), the current one chosen; picking one is `/model <id>`.
- `cmd-error-buttons` an error comes with a Retry button (`retry:<session id>`) and the model select. Bots cannot click: the click runs the same `!retry` / `!model <id>`, with the clicked message as the reply target.
- `cmd-model` `/model <id> [message]` continues in a new session on that model, same workers; the thread follows it.
- `cmd-model-fast` three `/model` in a row in one thread each answer within seconds; the bot does not stall.
- `cmd-continue` `/continue [message]` continues in a new session with the same config.
- `cmd-retry` `/retry` steps a failed session again.
- `cmd-stop` `/stop` stops the running step.
- `cmd-info` `/info` shows status, model, workers and context.

## How to get to it (user POV)

- Type the command as a message in a thread the bot made.

## Driving it with say.sh

Preconditions:

- A thread from new-session, whose session is idle.

- **Info.** Send `/info`. `scripts/thread.sh <thread id> 2` shows `session \`<id>\` <status>`, the model, workers and context.
- **List models.** Send `/model`. `scripts/discord.sh "/channels/<thread id>/messages?limit=1" | jq '.[0].components[0].components[0] | {custom_id, options: [.options[] | {value, default}]}'` lists the preset ids, one `default: true`.
- **Error buttons.** A stop notice has no `components`: a stop is on purpose, and a retry after it only adds the harness's `Continue`, which the agent may not act on. A failure's notice has a `retry:<id>` button and the `model:<id>` select; a failure is hard to cause on demand, so check it in code or when one happens.
- **Switch, three times fast.** Send `/model <id A> reply with the word a`, then `/model <id B> reply with the word b`, then `/model <id A> reply with the word c`, each as soon as the last is sent. Each gets `-# continuing in session <new id> on <model>` within seconds, and `a`, `b`, `c` come back in order. `jq '.threads["discord:<thread id>"]'` on the state file is the third new id. The thread's name is unchanged.
- **Continue.** Send `/continue reply with the word d`. A `continuing in session` line on the same model, then `d`.
- **Stop.** Send `use the run tool to execute: sleep 120 on admin, then reply done`, wait for the `-# run on admin: sleep 120` line, then send `/stop`. A `⚠️` line says the session was stopped by discord; `mainplane info` shows `failed`.
- **Retry.** Send `/retry`. `mainplane info` shows `stepping`, and the bot relays the step again (`-# run on admin: sleep 120`); send `/stop` to end it.

## Gotchas

- `/model` with an unknown id answers `/model failed: no connected model`; ids are the select's option values.
- Each switch makes a session; delete them in cleanup.
- The tester sends plain messages; a person typing `/model` in the Discord client may see Discord's own command picker, and Enter still sends it as text.
