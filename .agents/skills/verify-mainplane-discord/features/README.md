# mainplane-discord verification map

The user-facing behavior of the mainplane-discord bot, one file per feature. Read this, then use the feature's file as the recipe.

## Baseline preconditions

- The build under test is running: `scripts/doctor.sh` passes from the checkout it was built from.
- Chrome is running on the Mac, signed in to Discord, with remote debugging allowed; `CHROME_DEVTOOLS_AXI_AUTO_CONNECT=1 CHROME_DEVTOOLS_AXI_SESSION=verify-mpd chrome-devtools-axi pages` lists its tabs.
- `#bot-test` exists in the Mainplane guild. Every test message goes there or in threads under it.
- At least one provider is connected (`mainplane providers` on the server).

## Driving conventions

- Type as the user, in Discord, with `chrome-devtools-axi`: open the channel or thread URL, `snapshot`, `fill` the textbox named `Message #<channel or thread name>`, `press Enter`.
- Read results with `scripts/thread.sh`, not the screen; the screen is evidence only.
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
- [catch-up.md](catch-up.md): what happened while the bot was down is picked up.
