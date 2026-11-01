# New session

A message in a channel starts a Mainplane session on the model the terminal app would pick, with every connected worker, in a new thread named `<session id> <first words>`. Messages in the thread continue it; the agent's replies and tool calls come back there.

## Sub-features

- `new-start` a channel message makes a session and a thread.
- `new-model` the model is the newest session's, else the catalog's default, if its provider is connected.
- `new-workers` the session has every connected worker.
- `new-reply` the agent's reply comes back in the thread; a thread message continues the session.

## How to get to it (user POV)

- Send a message in any text channel of the guild.
- Send a message in a thread the bot made.

## Driving it with say.sh

Preconditions:

- Baseline preconditions hold.

- **Start.** In `#bot-test`, send `verify new-session <run id>: reply with the word ok`. Run `say.sh <bot-test id> "<text>"`. Within seconds `scripts/thread.sh <bot-test id> 3` shows the message with `[thread <id>]`, and the thread's name starts with an 8-hex session id.
- **Config.** Run `mainplane info <session id>` on the server. Its model matches `mainplane sessions` newest before the run (or the catalog default), and its workers equal `mainplane workers` minus refused ones.
- **Reply.** Run `scripts/thread.sh <thread id>`. The bot posted the agent's `ok`.
- **Continue.** In the thread, send `reply with the word again`. `scripts/thread.sh <thread id>` shows `again` from the bot, and `mainplane tail <session id>` shows both messages `via=discord`.

## Gotchas

- The thread is made on the user's message; its id is the message's `thread.id`, not the message id.
- The first reply can take tens of seconds on a cold provider; wait on `thread.sh`, not on the screen.
- Text starting with `/` is a command only when its first word is one of the bot's; `/data/x is broken` is a message.
