---
name: verify-mainplane-discord
description: Drive the mainplane-chat bot the way its user does, by typing in Discord or Slack, and keep evidence of what it did. Use to prove a change to the bot works, before a PR is ready.
---

# Verify mainplane-chat

The bot runs on the harness machine (denguinho-server) as the systemd user service `mainplane-chat`, from `~/go/bin/mainplane-chat`. A user drives it by typing in the Discord server "Mainplane"; it talks to the Mainplane harness at `http://127.0.0.1:8080`. Verification types as a separate tester bot, "Mainplane Tester", which the bot handles like a person while its env file sets `MAINPLANE_DISCORD_TESTER` to the tester's user id. Never automate a person's Discord account: Discord bans self-bots.

| Thing | Value |
| --- | --- |
| Guild | `1565949504866222130` (Mainplane) |
| Test channel | `#bot-test` under Text Channels; only test messages go there |
| Bot user | `1565948277302624298` |
| Tester | its token in `~/.config/mainplane-chat/tester.env` (`TESTER_TOKEN=`), mode 600 |
| State file | `~/.local/state/mainplane-chat/state.json` on the server |
| Logs | `journalctl --user -u mainplane-chat` on the server |

The helpers in `scripts/` run on the server, from a checkout of this repo. From the Mac: `ssh denguinho-server 'cd <checkout> && .agents/skills/verify-mainplane-discord/scripts/<helper>'`.

## Launch

Build the branch under test and swap it in; keep the previous binary to restore.

```sh
cp -p ~/go/bin/mainplane-chat ~/go/bin/mainplane-chat.prev
go build -o ~/go/bin/mainplane-chat . && systemctl --user restart mainplane-chat
```

Ready when the journal shows `caught up` after the restart (`scripts/doctor.sh` checks it). Teardown: restore `.prev` and restart, if the change is not being kept.

## Doctor

`scripts/doctor.sh` (read-only): the service is active, the binary was built from this checkout's HEAD, the bot caught up since it started, the harness answers, and the state file parses.

## Drive

Post as the tester, read as the bot, check Mainplane with its CLI, all on the server:

```sh
s=.agents/skills/verify-mainplane-discord/scripts
$s/say.sh <channel or thread id> "/info"      # prints the message id
$s/thread.sh <channel or thread id> 5         # what the bot said, oldest first
mainplane info <session id>; mainplane tail <session id>
```

A message in `#bot-test` starts a session and a thread on that message; `thread.sh` on the channel shows the thread id next to it. Wait on `thread.sh` (poll every few seconds), not a fixed sleep: agent replies take seconds to minutes.

## Evidence

For each feature driven, keep: the messages typed (time, text), `scripts/thread.sh` output after, the journal lines for the run, the state file's entry for the thread (`jq '.threads["discord:<thread id>"]'`), and `mainplane info` of each session involved. Put them under `/tmp/verify-mainplane-discord/<run>/` on the server.

## Cleanup

Delete only what the run made: its test sessions (`mainplane delete <id>`, after saving evidence) and its threads (archive from Discord). Keep `#bot-test`. Restore `~/go/bin/mainplane-chat.prev` if the build under test is not being kept. Leave `MAINPLANE_DISCORD_TESTER` set: it only lets the tester in.

## Features

See `features/README.md`.
