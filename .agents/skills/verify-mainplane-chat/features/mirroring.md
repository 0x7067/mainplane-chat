# Mirroring sessions made elsewhere

Mainplane syncs sessions across every connector, so a session made outside Discord (terminal app, CLI, another agent) gets its own thread in `#sessions`: a post "`<id>` · via `<source>` · `<model>`" with the thread on it, named `<id> <first words>`. The thread relays the agent and shows messages sent from elsewhere quoted with their source; messages typed in it continue the session. The post shows how the last turn went, as a start message does: 👀 while the agent works, then ✅ or ❌. Only sessions made after mirroring began (`mirror_since` in the state file) are mirrored. Deleting the session deletes the thread and its post, so `#sessions` lists what the terminal app lists.

## Sub-features

- `mirror-new` a session made with the CLI gets a thread in `#sessions` within a few seconds, its first message quoted and its reply relayed.
- `mirror-quote` a later message sent from the CLI shows in the thread as `> **cli:** …`; in any thread, Discord messages are not repeated.
- `mirror-continue` a message typed in the mirrored thread continues the session.
- `mirror-mark` the post gets 👀 while a turn runs, asked from anywhere, then ✅, or ❌ when it fails or is stopped.
- `mirror-delete` deleting the session deletes its thread and post.

## How to get to it (user POV)

- Start a session anywhere but Discord: `mainplane` (terminal app), `mainplane new` + `mainplane message`, or an agent running those.
- Open `#sessions` in Discord.

## Driving it with say.sh

Preconditions:

- Baseline preconditions hold, and a `#sessions` text channel exists (`scripts/discord.sh /guilds/<guild>/channels`).

- **New.** On the server: `printf '%s' '{"model":"claude-code/claude-haiku-5-5","preset":"claude-code/claude-haiku-5-5","tools":"default","context_limit":250000,"workers":[{"name":"<worker>"}],"params":{"effort":"low"}}' | mainplane new` prints the id; `mainplane message <id> "verify mirroring: reply with just the word one"`. Within about 5 seconds `scripts/thread.sh <#sessions id> 3` shows `` `<id>` · via cli · claude-code/claude-haiku-5-5 `` with `[thread <thread id>]`; `scripts/thread.sh <thread id>` shows `> **cli:** verify mirroring: …` then `one`. `scripts/discord.sh /channels/<#sessions id>/messages/<thread id> | jq -c '[.reactions[] | select(.me) | .emoji.name]'` is `["✅"]`.
- **Quote.** `mainplane message <id> "reply with just the word two"`. The thread shows `> **cli:** reply with just the word two`, then `two`.
- **Continue.** `scripts/say.sh <thread id> "reply with just the word three"`. The thread shows `three`; `mainplane tail <id>` has the message `via=discord`, and the thread does not repeat it quoted.
- **Delete.** `mainplane delete <id>`. Within about a minute `scripts/discord.sh /channels/<thread id> | jq .thread_metadata` fails with 404 Unknown Channel, and the post is gone from `scripts/thread.sh <#sessions id>`.

## Gotchas

- A session is checked once it has its first message; one made but never messaged gets no thread yet.
- Continuations of a session a thread shows move that thread instead of making a new one (see continuations.md).
- Every session an agent makes with the CLI is mirrored too, test sessions included; delete them after a run.
