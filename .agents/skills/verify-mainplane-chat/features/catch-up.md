# Catch-up after downtime

When the bot starts, it relays agent replies it missed and handles messages typed while it was down, in channels and threads it had seen before.

## Sub-features

- `catch-relay` replies the agent wrote while the bot was down come back in the thread.
- `catch-messages` thread messages typed while it was down reach the session, in order.
- `catch-start` a channel message typed while it was down starts its session and thread.
- `catch-map` a thread moved by /model or a continuation still follows the new session after a restart.

## How to get to it (user POV)

- Use Discord while the bot is stopped, or while the server restarts; it catches up when it is back.

## Driving it with say.sh

Preconditions:

- A thread from thread-commands, moved to a new session at least once.

- **Stop the bot.** On the M4: `launchctl bootout gui/$(id -u)/local.mainplane-chat` (`KeepAlive` would restart a plain kill). On systemd: `systemctl --user stop mainplane-chat`.
- **Send while down.** In the thread, send `reply with the word late`. In `#bot-test`, send `verify catch-up <run id>: reply with the word fresh`.
- **Start the bot.** On the M4: `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/local.mainplane-chat.plist`, then check the log for `caught up`. On systemd: `systemctl --user start mainplane-chat`; `scripts/doctor.sh` passes.
- **Proof.** `scripts/thread.sh <thread id>` shows `late` from the bot, and `mainplane tail <state file's session for the thread>` shows the message `via=discord`, so the moved mapping held. `scripts/thread.sh <bot-test id> 3` shows the `fresh` message with a new thread, which has `fresh`.

## Gotchas

- A channel or thread the bot never saw starts from its newest message; nothing typed there earlier is replayed.
- Archived threads are not resumed until a message unarchives them.
- Stopping the bot is a shared-system change; start it again even if the run fails.
