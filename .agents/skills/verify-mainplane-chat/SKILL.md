---
name: verify-mainplane-chat
description: Drive the mainplane-chat bot the way its user does, by typing in Discord or Slack, and keep evidence of what it did. Use to prove a change to the bot works, before a PR is ready.
---

# Verify mainplane-chat

The live bot runs on the harness machine, the M4 Mac (`ssh pedrostestm4@Inventory-MYFQJGWG95`), as the user LaunchAgent `local.mainplane-chat` (`~/Library/LaunchAgents/local.mainplane-chat.plist`), from `~/.local/bin/mainplane-chat`. It talks to the Mainplane harness at `http://127.0.0.1:8080`. Its env sets Slack tokens and no Discord token, so the Discord steps below do not apply there unless a Discord bot is added. A user drives the bot by typing in a private Discord server (or Slack on the M4). Verification types as a separate tester bot, "Mainplane Tester", which the bot handles like a person while its env file sets `MAINPLANE_DISCORD_TESTER` to the tester's user id. Never automate a person's Discord account: Discord bans self-bots.

| Thing | Value |
| --- | --- |
| Guild | the test server (`scripts/discord.sh /users/@me/guilds`) |
| Test channel | `#bot-test` under Text Channels; only test messages go there |
| Bot user | `scripts/discord.sh /users/@me` |
| Tester | `~/.config/mainplane-chat/tester.env`, mode 600: `TESTER_TOKEN=` its token, `SLACK_DM=` the user's DM channel with the Slack app |
| State file | `~/.local/state/mainplane-chat/state.json` on the server |
| Logs | `~/Library/Logs/mainplane-chat.log` on the server (the LaunchAgent's stdout and stderr); on systemd, `journalctl --user -u mainplane-chat` |

The helpers in `scripts/` run on the server, from a checkout of this repo.

## Launch

Build the branch under test and swap it in; keep the previous binary to restore.

```sh
cp -p ~/.local/bin/mainplane-chat ~/.local/bin/mainplane-chat.prev
go build -o ~/.local/bin/mainplane-chat . && launchctl kickstart -k gui/$(id -u)/local.mainplane-chat
```

Ready when the log shows `caught up` after the restart. Teardown: restore `.prev` and kickstart again, if the change is not being kept. On a systemd host, the same steps use `~/go/bin/mainplane-chat` and `systemctl --user restart mainplane-chat`.

## Doctor

`scripts/doctor.sh` (read-only) checks the LaunchAgent and its log file on macOS (the M4), and `systemctl --user` and `journalctl` elsewhere. It checks that the service is active, the binary was built from this checkout's HEAD, the bot caught up since it started, the harness answers, and the state file parses.

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

Delete only what the run made: its test sessions (`mainplane delete <id>`, after saving evidence) and its threads (archive from Discord). Keep `#bot-test`. Restore `~/go/bin/mainplane-chat.prev` if the build under test is not being kept. Leave `MAINPLANE_DISCORD_TESTER` set: it only lets the tester in.

## Features

See `features/README.md`.
