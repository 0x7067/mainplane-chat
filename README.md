# mainplane-discord

Drive [Mainplane](https://github.com/mainplane-ai/mainplane) agents from a private Discord server.

- A message in any channel starts a session in a new thread named `<session id> <first words>`. Its model is picked as the mainplane terminal app picks it (the newest session's, else the catalog's default, if its provider is connected); its workers are every connected worker.
- Messages in that thread continue the session; the agent's replies and tool calls come back there.
- Thread commands, as in the terminal app and CLI (`!` works too):
  - `/model` lists the connected models; `/model <id> [message]` continues in a new session on that model, with the same workers. Use it when a provider's quota runs out.
  - `/continue [message]` continues in a new session with the same config.
  - `/retry` steps a failed session again; `/stop` stops the running step; `/info` shows status, model, workers and context.
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
