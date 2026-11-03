---
name: verify-mainplane-chat
description: Drive the mainplane-chat bot the way its user does, by typing in Discord or Slack, and keep evidence of what it did. Use to prove a change to the bot works, before a PR is ready.
---

# Verify mainplane-chat

Verify on a host that runs the bot and has the tester. A user drives the bot by typing in a private Discord server (or Slack). Verification types as a separate tester bot, "Mainplane Tester", which the bot handles like a person while its env file sets `MAINPLANE_DISCORD_TESTER` to the tester's user id. Never automate a person's Discord account: Discord bans self-bots.

## Deployment

This repo does not know where the bot runs. Each host that runs it describes its deployment in `~/.config/mainplane-chat/deploy.sh`, a bash file kept with that host's setup (not here), which the scripts and steps below source:

| Name | What it is |
| --- | --- |
| `MPC_BIN` | the binary the service runs; builds go there with `go build -o "$MPC_BIN" .` |
| `mpc_running` | exits 0 while the service runs |
| `mpc_start`, `mpc_stop` | start and stop the service (stop must hold: a supervisor that restarts a killed process needs its own stop) |
| `mpc_log_since_start` | prints the bot's log since the running process started |

A systemd user service, for example: `MPC_BIN=$HOME/go/bin/mainplane-chat`, `mpc_running() { systemctl --user is-active -q mainplane-chat; }`, `mpc_start`/`mpc_stop` with `systemctl --user start|stop`, `mpc_log_since_start() { journalctl --user -u mainplane-chat -o cat --no-pager --since "$(systemctl --user show -p ActiveEnterTimestamp --value mainplane-chat)"; }`.

| Thing | Value |
| --- | --- |
| Guild | the test server (`scripts/discord.sh /users/@me/guilds`) |
| Test channel | `#bot-test` under Text Channels; only test messages go there |
| Bot user | `scripts/discord.sh /users/@me` |
| Tester | `~/.config/mainplane-chat/tester.env`, mode 600: `TESTER_TOKEN=` its token, `SLACK_DM=` the user's DM channel with the Slack app |
| State file | `~/.local/state/mainplane-chat/state.json` on the server |

The helpers in `scripts/` run on the server, from a checkout of this repo.

## Launch

Build the branch under test and swap it in; keep the previous binary to restore.

```sh
. ~/.config/mainplane-chat/deploy.sh
cp -p "$MPC_BIN" "$MPC_BIN.prev"
go build -o "$MPC_BIN" . && mpc_stop && mpc_start
```

Ready when `mpc_log_since_start` shows `caught up`. Teardown, if the change is not being kept: `mpc_stop && cp -p "$MPC_BIN.prev" "$MPC_BIN" && mpc_start` (stop first: Linux refuses to overwrite a running binary, "Text file busy"). Before any of it, try the build with `-console` (README, "Trying a build").

## Doctor

`scripts/doctor.sh` (read-only) checks, through the host's `deploy.sh`, that the service runs, the bot caught up since it started, the binary was built from this checkout's HEAD, the harness answers, and the state file parses.

## Drive

Post as the tester, read as the bot, check Mainplane with its CLI, all on the server:

```sh
s=.agents/skills/verify-mainplane-chat/scripts
$s/say.sh <channel or thread id> "/info"      # prints the message id
$s/thread.sh <channel or thread id> 5         # what the bot said, oldest first
mainplane info <session id>; mainplane tail <session id>
```

A message in `#bot-test` starts a session and a thread on that message; `thread.sh` on the channel shows the thread id next to it. Wait on `thread.sh` (poll every few seconds), not a fixed sleep: agent replies take seconds to minutes.

## Evidence

For each feature driven, keep: the messages typed (time, text), `scripts/thread.sh` output after, the bot's log for the run (`mpc_log_since_start`), the state file's entry for the thread (`jq '.threads["discord:<thread id>"]'`), and `mainplane info` of each session involved. Put them under `/tmp/verify-mainplane-chat/<run>/` on the server.

## Cleanup

Delete only what the run made: its test sessions (`mainplane delete <id>`, after saving evidence) and its threads (archive from Discord). Keep `#bot-test`. Restore `$MPC_BIN.prev` if the build under test is not being kept. Leave `MAINPLANE_DISCORD_TESTER` set: it only lets the tester in.

## Features

See `features/README.md`.
