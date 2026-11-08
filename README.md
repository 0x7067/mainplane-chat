# mainplane-chat

**Talk to your [Mainplane](https://github.com/mainplane-ai/mainplane) agents from Discord and Slack.** Every session is a thread. You see the agent work live, stop it with a button, send it files, and get files back. Long tasks keep going past the context limit on their own.

<p align="center">
  <img src="docs/discord.gif" alt="A Discord thread: a message starts a Mainplane session, a live status message lists its tool calls, and the agent replies with an attached chart" width="860">
</p>

[Mainplane](https://github.com/mainplane-ai/mainplane) is an open-source agent harness. It runs agents on any model and lets them control your computers. You message the agents through connectors, such as its terminal app and CLI. mainplane-chat adds Discord and Slack as connectors. It is a small Go service that runs next to your Mainplane server and talks to its API.

```
 Discord server ─┐                                   ┌─ worker: server
                 ├─ mainplane-chat ─ Mainplane server ┼─ worker: laptop
 Slack DMs ──────┘                                   └─ worker: ...
```

> mainplane-chat is a community project. It is not made by or affiliated with the Mainplane team.

## Features

- **A thread per session.** A message in a channel (Discord), or a DM or a mention of the app in a Slack channel or group DM, starts a session in a thread. Replies in the thread continue it (in a Slack channel or group DM, only replies that mention the app).
- **Live status.** One message shows the running turn: its time, its last tool calls and a Stop button. When the turn ends, it shrinks to one line: `14 calls · 2m31s · 38% context`.
- **Files in both directions.** Send images and text files with a message. The agent sends a file back with a standalone line `attach: /path/to/file` in its reply (not shell output or a code block). In Slack, `attach-channel: /path/to/file` shares it as a top-level file in the same channel.
- **Executed Slack actions.** Agents can run `mainplane-chat slack post --text '...' [--file /path]` to create a real top-level channel message, or `slack reply` for the source thread. The sender resolves the destination from `MAINPLANE_SESSION`, executes the API call, and returns a confirmed message permalink. Retrying the same payload for one request reuses its durable receipt.
- **Checked delivery.** File delivery finishes before the turn gets ✅. Read/upload failures fail the turn, and so does a reply, quote or job report that could not be posted. If the model only prints an attachment directive from `run`, the bridge suppresses its unsupported final claim and requests one corrective reply; tool stdout itself never uploads files. Slack top-level uploads get a receipt back in the source thread.
- **Every connector, one conversation.** Sessions started in the terminal app or CLI get a thread in `#sessions`. Messages sent from elsewhere show in the thread with their source.
- **Recovery.** A failed step comes with a Retry button and a model picker. When a provider's quota runs out, continue on another model in one click.
- **Context recovery.** When a session fills its context, the bot continues it in a new session with a bounded handoff of the requests and recent tool results. The thread follows. Provider context errors count as full when the history, including the last generated output, filled the window; a length-limited step that fills the session also continues. One oversized message keeps Retry. Retry recovers a full session even after downtime. If a fresh automatic continuation fills before making progress, the bot stops rather than looping.
- **Catches up.** What happens while the bot is down is picked up when it starts again, and a message that failed to reach its session is tried again then too.
- **Slack agent sessions.** In Slack, a thread is a native agent session, with its title, working state and Stop button.

<p align="center">
  <img src="docs/slack.gif" alt="A Slack DM with the Mainplane app: an agent session thread with its title, a status line and the agent's reply" width="860">
</p>

<sub>The demos are re-creations of real sessions, with the worker names changed.</sub>

## Setup

You need a running [Mainplane server](https://github.com/mainplane-ai/mainplane#self-hosting-quickstart) and Go 1.27 or later. Run mainplane-chat on the same machine as the server. Use Discord, Slack or both.

1. Make a harness key for the bot:

   ```sh
   mainplane-server key new chat
   ```

2. For Discord, make a bot in the [Developer Portal](https://discord.com/developers/applications). Turn on the **Message Content** intent. Invite it to a private server with permission to read and send messages, manage threads, add reactions and attach files. Make a text channel named `sessions` for sessions started outside Discord.

3. For Slack, make an app [from the manifest](https://api.slack.com/apps?new_app=1) in [`slack-manifest.yaml`](slack-manifest.yaml) and install it to your workspace. Make an app-level token with `connections:write` for Socket Mode. An app made from an older manifest needs the new events and scopes, then a reinstall, before it hears channels and group DMs. At start, the bot logs each scope its app lacks and what goes without it: `slack: the app lacks users:read: names show as user ids, ...`.

4. Install and run:

   ```sh
   go install github.com/0x7067/mainplane-chat@latest
   mainplane-chat
   ```

   To keep it running, use a systemd user service or similar, with the environment below in an env file. On macOS, use a user LaunchAgent with `RunAtLoad` and `KeepAlive`, whose program is `/bin/zsh -c 'set -a; source $HOME/.config/mainplane-chat/env; exec $HOME/.local/bin/mainplane-chat'`, and restart it with `launchctl kickstart -k gui/$(id -u)/<label>`.

| Variable | |
| --- | --- |
| `MAINPLANE_KEY` | the harness key from step 1 |
| `MAINPLANE_URL` | the harness URL; default `http://127.0.0.1:8080` |
| `DISCORD_TOKEN` | the Discord bot token; leave it unset to run Slack only |
| `SLACK_BOT_TOKEN` | the `xoxb-` token of the Slack app; leave it unset to run Discord only |
| `SLACK_APP_TOKEN` | the `xapp-` app-level token, with Slack |
| `MAINPLANE_SLACK_USERS` | with Slack: comma-separated Slack user ids the bot takes messages from; `*` adds every full member of the bot's workspace (no guests, Slack Connect users, bots or deactivated accounts) |
| `MAINPLANE_DISCORD_TESTER` | optional: the user id of a tester bot whose messages the bot takes as a person's, for end-to-end tests. Other bots stay ignored |

The bot keeps its state in `$XDG_STATE_HOME/mainplane-chat/state.json` (`~/.local/state` by default).

**Security:** anyone who can post in the Discord server, or any Slack user `MAINPLANE_SLACK_USERS` allows (with `*`, the whole workspace), controls your agents and every worker they reach. Use a private Discord server.

## Commands

Type these in a thread. In Discord, `/` and `!` both work. In Slack, use `!`, as Slack takes `/` for its own commands.

| Command | |
| --- | --- |
| `/model` | show a picker of the connected models: the catalog's, then other models recent sessions used |
| `/model <id> [message]` | continue in a new session on that model, with the same workers. Any `<provider>/<model>` of a connected provider works, catalog or not, with the session's tools and context limit |
| `/continue [message]` | continue in a new session with the same config |
| `/retry` | step a failed session again |
| `/stop` | stop the running step |
| `/info` | show the status, model, workers and context |
| `/busy` | list other active sessions, their authors and outstanding background jobs |
| `/delete` | delete the session for good, as `mainplane delete` does |

## Trying a build

`mainplane-chat -console` runs the bridge in the terminal against the harness, with no chat tokens and a throwaway state file, so the running bot is untouched. Each stdin line is a message in one thread: the first starts a session (or `-session <id>` shows an existing one), the rest are messages and thread commands. It prints what the bot would post: `say:` replies, `react` marks, and `status:` lines titled as in Slack, with each call's plan status at the end of a turn. At the end of input it waits (`-wait`, default 10m) for the session to stop working.

```sh
printf '!info\n' | MAINPLANE_KEY=... mainplane-chat -console -session 5b733adb
printf 'list the files in /tmp\n' | MAINPLANE_KEY=... mainplane-chat -console
```

## Details

### Slack sender for agents

Make the binary and its existing `SLACK_BOT_TOKEN` available to `run` on the bridge's worker, usually with a deployment-owned wrapper that loads the service env without showing it. The sender needs no app token, harness key, new Slack scope, or listener. Tell agents which worker runs it. Normal final text still replies to the source thread; outgoing actions are executed commands:

```sh
mainplane-chat slack post --text 'A new channel message'
mainplane-chat slack post --file /absolute/path/image.png --text 'Caption'
mainplane-chat slack reply --text 'A reply in the source thread'
mainplane-chat slack receipt --key <receipt-id>
```

`post` never accepts a thread target. `reply` defaults to the source thread. `--channel` selects another channel the bot can already access; an explicit reply destination also needs `--thread`. An ambiguous/missing session mapping fails instead of guessing. `--session` overrides `MAINPLANE_SESSION` for operator use.

Only `ok:true`, `state:confirmed` and a `permalink` are success. If a response is uncertain, inspect its receipt rather than sending again. Same-payload retries within one chat request are deduplicated across continuations; for an intentionally separate repeat, use a distinct `--key <name>`. Receipts under the state directory's `slack-actions/` contain metadata and content hashes, not message/file contents or credentials. The bridge validates this journal before treating a tool result as delivery evidence; arbitrary stdout is not a receipt.

<details>
<summary>Discord</summary>

- A message in any channel starts a session in a new thread named `<session id> <first words>`. Its model is the one of the newest session a thread shows (or, before any thread has one, the newest session's), else the catalog's default, if its provider is connected. A model outside the catalog, such as a self-hosted provider's, starts with that session's config. Sessions made elsewhere, such as test runs, do not move the chats to their model. Its workers are every connected worker.
- A session made outside Discord (terminal app, CLI, another agent) gets its own thread in `#sessions`, from when mirroring began. The bot finds the channel by name.
- Messages sent from elsewhere show quoted with their source (`> **cli:** …`), and a background job's report shows as one line (`-# job on admin exited 0 after 40s`).
- A thread whose session is deleted is archived and locked. In `#sessions`, its post is deleted too, so the channel lists live sessions. When a session gets a name, its thread is renamed `<id> <name>`.
- The final reply is a Discord reply to the message it answers. It notifies you when the turn took over two minutes.
- Each message sent to a session gets a reaction: 👀 while the agent works, then ✅ or ❌. The start messages in a channel, and the posts in `#sessions`, show how each session's last turn went.
- A Retry button, model picker or Stop names the session it was posted with; once the thread has left that session, it says so and does nothing, as in Slack.
- The bot reads an `attach:` file on its own machine, up to 20 MiB, and uploads it in place of the line.
- Files sent with a message go to the session: images as images (up to 5 MiB), text files inline (up to 200 KiB). Others are listed as not sent.
- A thread follows a continuation made anywhere: `/model` or `/continue` in the terminal app moves the thread to the new session too. Mainplane has no event for new sessions, so the bot checks the newest ones every 3 seconds, as the terminal app does.

</details>

<details>
<summary>Slack</summary>

- A top-level DM to the app starts a session in its thread, which is a Slack agent session. Slack shows its title (the first words, then the session's name), its working state and a Stop button. A title changed in Slack names the session.
- In a group DM, private channel or channel the app is in, it hears only messages that mention it, in its own threads too, so people can talk there without the agent answering each message. A mention in a thread it runs continues that session; elsewhere it starts one in the thread it is in (its own thread, or the thread it replies in). The mention is left out of the text the agent gets. Other chatter is ignored.
- The agent gets each Slack message as `[<name> <@id>]: <text>`, and a session's first message opens with a note that it is a Slack app, where the thread is (DM, group DM and who is in it, or #channel), and that its replies are Slack messages. Thread commands (`!model`, `/stop`, ...) are not framed.
- Each message also names its exact current Mainplane session, Slack channel and thread. Other sessions remain separate conversations; the agent can inspect them with `mainplane sessions` and `mainplane info`, rather than mistaking the newest session for itself.
- Only users in `MAINPLANE_SLACK_USERS` are heard; with `*`, so is every full member of the workspace (checked with `users.info`, cached ten minutes). A reply also sent to the DM counts.
- Status, quotes, job lines, files, `attach:`, Retry, the model picker, auto-continue and following continuations work as in Discord. Remote files (shared from outside Slack) are not fetched.
- `attach-channel:` uploads a file at the top level of this same channel (or DM); ordinary replies and `attach:` remain in the thread. The file must be on the bridge's machine, not just another worker. Old Retry/model controls cannot restart a session the thread has already left.
- The finished status line carries the session id, as the Slack title does not. Messages get ✅ or ❌ when their turn ends. A failed step marks the agent session suspended.
- Sessions made in Slack stay in Slack: `#sessions` mirrors only sessions made outside the chats.

</details>

## License

MIT. Built on [Mainplane](https://github.com/mainplane-ai/mainplane), which is MIT too.
