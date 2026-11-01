---
name: verify-mainplane-discord
description: Drive the mainplane-discord bot the way its user does, by typing in Discord, and keep evidence of what it did. Use to prove a change to the bot works, before a PR is ready.
---

# Verify mainplane-discord

The bot runs on the harness machine (denguinho-server) as the systemd user service `mainplane-discord`, from `~/go/bin/mainplane-discord`. A user drives it by typing in the Discord server "Mainplane"; it talks to the Mainplane harness at `http://127.0.0.1:8080`. The bot ignores messages from bots, so driving it means typing as the user, in their signed-in browser.

| Thing | Value |
| --- | --- |
| Guild | `1565949504866222130` (Mainplane) |
| Test channel | `#bot-test` under Text Channels; only test messages go there |
| Bot user | `1565948277302624298` |
| State file | `~/.local/state/mainplane-discord/state.json` on the server |
| Logs | `journalctl --user -u mainplane-discord` on the server |

The helpers in `scripts/` run on the server, from a checkout of this repo. From the Mac: `ssh denguinho-server 'cd <checkout> && .agents/skills/verify-mainplane-discord/scripts/<helper>'`.

## Launch

Build the branch under test and swap it in; keep the previous binary to restore.

```sh
cp -p ~/go/bin/mainplane-discord ~/go/bin/mainplane-discord.prev
go build -o ~/go/bin/mainplane-discord . && systemctl --user restart mainplane-discord
```

Ready when the journal shows `caught up` after the restart (`scripts/doctor.sh` checks it). Teardown: restore `.prev` and restart, if the change is not being kept.

## Doctor

`scripts/doctor.sh` (read-only): the service is active, the binary was built from this checkout's HEAD, the bot caught up since it started, the harness answers, and the state file parses.

## Drive

Discord, in the user's signed-in Chrome on the Mac, with `chrome-devtools-axi` attached to it. Chrome must be running with remote debugging allowed at `chrome://inspect/#remote-debugging`.

```sh
export CHROME_DEVTOOLS_AXI_AUTO_CONNECT=1 CHROME_DEVTOOLS_AXI_SESSION=verify-mpd
chrome-devtools-axi open https://discord.com/channels/1565949504866222130/<channel or thread id>
chrome-devtools-axi snapshot                 # find the message box: textbox "Message #bot-test"
chrome-devtools-axi fill @<uid> "/info" && chrome-devtools-axi press Enter
```

A message in `#bot-test` starts a session and a thread; open the thread by its id (from `scripts/thread.sh` on the channel, or the snapshot) to type in it. Read what the bot said with the API rather than the screen: `scripts/thread.sh <channel or thread id> [limit]`. The Mainplane side: `mainplane sessions`, `mainplane info <id>`, `mainplane tail <id>` on the server.

## Evidence

For each feature driven, keep: the messages typed (time, text), `scripts/thread.sh` output after, the journal lines for the run, the state file's entry for the thread (`jq '.threads["<thread id>"]'`), and `mainplane info` of each session involved. Put them under `/tmp/verify-mainplane-discord/<run>/` on the server.

## Cleanup

Delete only what the run made: its test sessions (`mainplane delete <id>`, after saving evidence) and its threads (archive from Discord). Keep `#bot-test`. Restore `~/go/bin/mainplane-discord.prev` if the build under test is not being kept. Close the `verify-mpd` page with `chrome-devtools-axi closepage`; never close the user's other tabs.

## Features

See `features/README.md`.
