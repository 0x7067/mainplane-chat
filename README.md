# mainplane-discord

Drive [Mainplane](https://github.com/mainplane-ai/mainplane) agents from a private Discord server.

- A message in any channel starts a session in a new thread named `<session id> <first words>`. Its model is picked as the mainplane terminal app picks it (the newest session's, else the catalog's default, if its provider is connected); its workers are every connected worker.
- Messages in that thread continue the session; the agent's replies and tool calls come back there.
- Thread commands, as in the terminal app and CLI (`!` works too):
  - `/model` lists the connected models; `/model <id> [message]` continues in a new session on that model, with the same workers. Use it when a provider's quota runs out.
  - `/continue [message]` continues in a new session with the same config.
  - `/retry` steps a failed session again; `/stop` stops the running step; `/info` shows status, model, workers and context.
- What happens while the bot is down is picked up when it starts: agent replies it missed, and messages sent in channels and threads it has seen before. Its state is in `$XDG_STATE_HOME/mainplane-discord/state.json` (`~/.local/state` by default).

Runs on the harness machine. Environment:

| Variable | |
| --- | --- |
| `DISCORD_TOKEN` | bot token; the bot needs the Message Content intent |
| `MAINPLANE_KEY` | harness api key (`mainplane-server key new discord`) |
| `MAINPLANE_URL` | default `http://127.0.0.1:8080` |

    go build -o ~/go/bin/mainplane-discord .
