---
name: verify-mainplane-chat
description: Drive the mainplane-chat bot the way its user does, by typing in Discord or Slack, and keep evidence of what it did. Use to prove a change to the bot works, before a PR is ready.
---

# Verify mainplane-chat

Two bots run this code, each beside its own Mainplane harness at `http://127.0.0.1:8080`:

| Host | Service | Binary | Chats | Logs |
| --- | --- | --- | --- | --- |
| admin (denguinho-server, Linux) | systemd user unit `mainplane-chat` | `~/go/bin/mainplane-chat` | Discord, and Slack as the app `mainplane` | `journalctl --user -u mainplane-chat` |
| M4 Mac (`ssh pedrostestm4@100.64.0.10`, Inventory-MYFQJGWG95) | LaunchAgent `local.mainplane-chat` | `~/.local/bin/mainplane-chat` | Slack only, as the app `symphony` | `~/Library/Logs/mainplane-chat.log` |

Verify on admin: it has the tester, a checkout (drive `dev`), `slack-axi` and the `mainplane` CLI. The M4 is the live bot people use; deploy there what passed on admin. The M4 has no checkout, `tester.env` or `slack-axi`, and its CLI is `/usr/local/bin/mainplane`, off a non-login ssh `PATH`.

A user drives the bot by typing in a private Discord server, or in Slack. Verification types as a separate tester bot, "Mainplane Tester", which the bot handles like a person while its env file sets `MAINPLANE_DISCORD_TESTER` to the tester's user id. Never automate a person's Discord account: Discord bans self-bots.

| Thing | Value |
| --- | --- |
| Guild | the test server (`scripts/discord.sh /users/@me/guilds`) |
| Test channel | `#bot-test` under Text Channels; only test messages go there |
| Bot user | `scripts/discord.sh /users/@me` |
| Tester | `~/.config/mainplane-chat/tester.env`, mode 600: `TESTER_TOKEN=` its token, `SLACK_DM=` the user's DM channel with the Slack app |
| Env file | `~/.config/mainplane-chat/env` |
| State file | `~/.local/state/mainplane-chat/state.json` |

The helpers in `scripts/` run on the server, from a checkout of this repo.

## Launch

Build the branch under test and swap it in; keep the previous binary to restore. Build with `go build -o`, never `go install`, which follows `GOBIN`. On admin:

```sh
cp -p ~/go/bin/mainplane-chat ~/go/bin/mainplane-chat.prev
go build -o ~/go/bin/mainplane-chat . && systemctl --user restart mainplane-chat
```

On the M4, from a checkout there:

```sh
cp -p ~/.local/bin/mainplane-chat ~/.local/bin/mainplane-chat.prev
go build -o ~/.local/bin/mainplane-chat . && launchctl kickstart -k gui/$(id -u)/local.mainplane-chat
```

Ready when the log shows `caught up` after the restart. Teardown: restore `.prev` and restart again, if the change is not being kept.

## Doctor

`scripts/doctor.sh` (read-only) checks `systemctl --user` and `journalctl` on Linux (admin), and the LaunchAgent and its log file on macOS (the M4). It checks that the service is active, the binary was built from this checkout's HEAD, the bot caught up since it started, the harness answers, and the state file parses.

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

For each feature driven, keep: the messages typed (time, text), `scripts/thread.sh` output after, the journal lines for the run, the state file's entry for the thread (`jq '.threads["discord:<thread id>"]'`), and `mainplane info` of each session involved. Put them under `/tmp/verify-mainplane-chat/<run>/` on the server.

## Cleanup

Delete only what the run made: its test sessions (`mainplane delete <id>`, after saving evidence) and its threads (archive from Discord). Keep `#bot-test`. Restore the binary's `.prev` if the build under test is not being kept. Leave `MAINPLANE_DISCORD_TESTER` set: it only lets the tester in.

## Features

See `features/README.md`.
