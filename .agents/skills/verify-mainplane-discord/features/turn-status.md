# Turn status

While the agent works on a turn, the thread shows one status message, edited in place: `**working** · started <t:…:R> · N calls`, the last three call lines, and a Stop button (`stop:<session id>`). At the end of the turn it becomes `-# N calls · 2m31s · 38% context`, or `-# failed after N calls · …` on an error (`stopped after` on a stop), with no button. Text the agent writes between calls goes out as plain messages; the final text goes out as a Discord reply to the message that began the turn. A turn with no calls has no status message.

## Sub-features

- `status-one` a turn with calls makes exactly one status message; no `-# <call>` line per call.
- `status-done` at the end the status is one `-#` line with calls, time and context, and no button.
- `reply-ref` in a thread, the final reply references the message it answers. The first turn's starter message is in the parent channel and Discord refuses cross-channel replies (`REPLIES_CANNOT_REFERENCE_OTHER_CHANNEL`), so the first reply has no reference.
- `reply-ping` a turn over two minutes notifies: `allowed_mentions.replied_user` on a reply, else a mention of the starter's author.
- `marks` each message sent to the session gets the bot's 👀, replaced by ✅ when the turn ends, or ❌ when it fails or is stopped. A start message is marked in the parent channel.
- `status-stop` the Stop button stops the session. A bot cannot click buttons: `!stop` drives the same code; the click itself needs a person.

## Driving it with say.sh

- **Status.** In a thread, send `run "sleep 20; echo hi" on admin twice, then reply with the word done`. While it runs, `scripts/discord.sh "/channels/<thread id>/messages?limit=3" | jq '.[] | {content, components}'` shows the status with the Stop button, edited (`edited_timestamp`) as calls come. After, `scripts/thread.sh <thread id> 5` shows `-# 2 calls · …` and `done`, and no `-# run on admin` lines.
- **Reply.** The `done` message has `message_reference.message_id` equal to the id `say.sh` printed.
- **Stop.** Send a long task (`run "sleep 120" on admin`), then `!stop`. The status becomes `-# stopped after 1 call · …`, then the stop notice.

- **Marks.** `scripts/discord.sh /channels/<channel>/messages/<id> | jq -c '[.reactions[] | select(.me) | .emoji.name]'` is `["👀"]` while the turn runs, then `["✅"]` (`["❌"]` after `!stop`). For a start message, `<channel>` is the parent channel.

## Gotchas

- `thread.sh` shows the first line of each message only; read the status with `discord.sh` and jq.
