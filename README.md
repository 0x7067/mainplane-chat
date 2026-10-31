# mainplane-discord

Drive [Mainplane](https://github.com/mainplane-ai/mainplane) agents from a private Discord server.

- A message in any channel starts a session (same model as the newest session, every connected worker) in a new thread named `<session id> <first words>`.
- Messages in that thread continue the session; the agent's replies and tool calls come back there.
- `!stop` in a thread stops the running step.

Runs on the harness machine. Environment:

| Variable | |
| --- | --- |
| `DISCORD_TOKEN` | bot token; the bot needs the Message Content intent |
| `MAINPLANE_KEY` | harness api key (`mainplane-server key new discord`) |
| `MAINPLANE_URL` | default `http://127.0.0.1:8080` |

    go build -o ~/go/bin/mainplane-discord .
