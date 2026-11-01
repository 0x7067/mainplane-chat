# mainplane-discord verification map

The user-facing behavior of the mainplane-discord bot, one file per feature. Read this, then use the feature's file as the recipe.

## Baseline preconditions

- The build under test is running: `scripts/doctor.sh` passes from the checkout it was built from.
- The tester bot is in the guild, `~/.config/mainplane-discord/tester.env` holds its token, and the bot's env sets `MAINPLANE_DISCORD_TESTER` to its user id.
- `#bot-test` exists in the Mainplane guild. Every test message goes there or in threads under it.
- At least one provider is connected (`mainplane providers` on the server).

## Driving conventions

- Type as the tester with `scripts/say.sh <channel or thread id> "<text>"`; read results with `scripts/thread.sh`.
- Start a fresh thread per feature, with a first message that says what the run is (`verify <feature> <run id>`).
- Agent replies cost model usage: keep prompts tiny, such as `reply with the word ok`.

## Proof and skip reporting

- Capture what was typed and when, `scripts/thread.sh` after, the journal lines for the run, and `mainplane info` of each session involved.
- A behavior that depends on the state file also needs the file's entry: `jq '.threads["<thread id>"]' ~/.local/state/mainplane-discord/state.json`.
- Report a step that could not run with the command tried and the precondition that was missing. Do not count a CLI-only check as proof of a Discord behavior.

## Features

- [new-session.md](new-session.md): a channel message starts a session and its thread.
- [thread-commands.md](thread-commands.md): /model, /continue, /retry, /stop, /info.
- [continuations.md](continuations.md): a thread follows a continuation made in the terminal app.
- [mirroring.md](mirroring.md): a session made outside Discord gets a thread in #sessions.
- [catch-up.md](catch-up.md): what happened while the bot was down is picked up.
