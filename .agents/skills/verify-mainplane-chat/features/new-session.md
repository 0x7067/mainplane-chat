# New session

A message in a channel starts a Mainplane session on the model the terminal app would pick, with every connected worker, in a new thread named `<session id> <first words>`. Messages in the thread continue it; the agent's replies and tool calls come back there.

## Sub-features

- `new-start` a channel message makes a session and a thread.
- `new-model` the model is the newest thread session's (else the newest session's), else the catalog's default, if its provider is connected; a model outside the catalog starts with that session's config.
- `new-workers` the session has every connected worker.
- `new-reply` the agent's reply comes back in the thread; a thread message continues the session.
- `new-label` when the session gets a name (its agent labels it at the start, per the Mainplane AGENTS.md), the thread is renamed `<id> <label>`.

## How to get to it (user POV)

- Send a message in any text channel of the guild.
- Send a message in a thread the bot made.

## Driving it with say.sh

Preconditions:

- Baseline preconditions hold.

- **Start.** In `#bot-test`, send `verify new-session <run id>: reply with the word ok`. Run `say.sh <bot-test id> "<text>"`. Within seconds `scripts/thread.sh <bot-test id> 3` shows the message with `[thread <id>]`, and the thread's name starts with an 8-hex session id.
- **Config.** Run `mainplane info <session id>` on the server. Its model matches the newest session a thread shows before the run (the newest in `mainplane sessions` whose id is in the state file's `threads`; else the newest; else the catalog default), and its workers equal `mainplane workers` minus refused ones.
- **Reply.** Run `scripts/thread.sh <thread id>`. The bot posted the agent's `ok`.
- **Continue.** In the thread, send `reply with the word again`. `scripts/thread.sh <thread id>` shows `again` from the bot, and `mainplane tail <session id>` shows both messages `via=discord`.

- **Label.** With a task the agent has to work on (not a one-word reply), within a minute `mainplane info <session id> | jq .name` is a 3-6 word label and `scripts/discord.sh /channels/<thread id> | jq -r .name` is `<session id> <label>`.

- **Elsewhere does not move it.** Make a session from the CLI on another connected model (`mainplane new`, no message), then send a new channel message: its session is still on the thread session's model. Delete the CLI session.
- **Outside the catalog.** On a harness with a self-hosted provider, after a thread session on `<provider>/<model>`, a new channel message's session has that model, tools and context limit.

## Gotchas

- Labels come from the agent: a small model on low effort may skip the rule. Opus follows it.

- The thread is made on the user's message; its id is the message's `thread.id`, not the message id.
- The first reply can take tens of seconds on a cold provider; wait on `thread.sh`, not on the screen.
- Text starting with `/` is a command only when its first word is one of the bot's; `/data/x is broken` is a message.
