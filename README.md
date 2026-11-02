# mainplane-discord

Drive [Mainplane](https://github.com/mainplane-ai/mainplane) agents from a private Discord server.

- A message in any channel starts a session in a new thread named `<session id> <first words>`. Its model is picked as the mainplane terminal app picks it (the newest session's, else the catalog's default, if its provider is connected); its workers are every connected worker.
- A session made outside Discord (terminal app, CLI, another agent) gets its own thread in `#sessions`, from when mirroring began, as Mainplane syncs sessions across every connector. Create a text channel named `sessions`; the bot finds it by name.
- Messages in a thread continue the session; the agent's replies come back there, and messages sent from elsewhere show quoted with their source (`> **cli:** …`). A thread whose session is deleted is archived and locked. When a session gets a name, its thread is renamed `<id> <name>`.
- While the agent works, one status message shows when the turn started, its tool calls (the last three) and a Stop button; it shrinks to `-# 14 calls · 2m31s · 38% context` when the turn ends. The final reply is a Discord reply to the message it answers, and notifies you when the turn took over two minutes.
- Each message sent to a session gets a reaction: 👀 while the agent works, then ✅ or ❌. The start messages in a channel show how each session's last turn went.
- An agent sends a file to the thread with a line `attach: /absolute/path` in its text; the bot reads the file on its own machine (up to 10 MiB) and uploads it in place of the line.
- Files sent with a message go to the session: images the model takes as images (up to 5 MiB), text files inline (up to 200 KiB). Others are listed as not sent.
- A failed step (not a stop) comes with a Retry button and a model picker; picking a model is `/model <id>`.
- Thread commands, as in the terminal app and CLI (`!` works too):
  - `/model` shows a picker of the connected models; `/model <id> [message]` continues in a new session on that model, with the same workers. Use it when a provider's quota runs out.
  - `/continue [message]` continues in a new session with the same config.
  - `/retry` steps a failed session again; `/stop` stops the running step; `/info` shows status, model, workers and context.
- When a thread's session ends at its context limit (`context N exceeds limit M`), the bot runs `/continue` itself, as Mainplane intends a full session to go on, and passes on the last message the full session did not answer. Continuations chain until the task is done.
- A thread follows a continuation made anywhere: `/model` or `/continue` in the terminal app on a thread's session moves the thread to the new session too. Mainplane has no event for new sessions, so the bot checks the newest ones every 3 seconds, as the terminal app does.
- What happens while the bot is down is picked up when it starts: agent replies it missed, and messages sent in channels and threads it has seen before. Its state is in `$XDG_STATE_HOME/mainplane-discord/state.json` (`~/.local/state` by default).

Runs on the harness machine. Environment:

| Variable | |
| --- | --- |
| `DISCORD_TOKEN` | bot token; the bot needs the Message Content intent |
| `MAINPLANE_KEY` | harness api key (`mainplane-server key new discord`) |
| `MAINPLANE_URL` | default `http://127.0.0.1:8080` |
| `MAINPLANE_DISCORD_TESTER` | optional: the user id of a tester bot whose messages the bot handles as a person's, for end-to-end tests (`.agents/skills/verify-mainplane-discord`). Other bots stay ignored |

    go build -o ~/go/bin/mainplane-discord .
