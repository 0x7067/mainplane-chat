# Continuations from anywhere

A thread follows a session that continues the one it shows, wherever it was made: the terminal app's `/model` and `/continue`, or any session whose first message starts `You are continuing session <id>.`. The bot checks the newest sessions every 3 seconds and posts `-# continued in session <id>`.

## Sub-features

- `cont-detect` a continuation made outside Discord moves the thread within seconds.
- `cont-relay` the new session's replies come back in the thread, from its start.
- `cont-chain` a continuation of a continuation moves it again.

## How to get to it (user POV)

- In the terminal app, open a thread's session (`mainplane <id>`) and use `/model` or `/continue`, then send.
- The CLI equivalent: `mainplane new` with the session's config, then `mainplane message <new id> "You are continuing session <old id>. ..."`.

## Driving it with say.sh

Preconditions:

- A thread from new-session, whose session is idle. The terminal is on the server, logged in (`mainplane status`).

- **Continue from the CLI.** On the server: `mainplane info <old id> | jq -c '.config | {model,tools,context_limit,workers,params,preset}' | mainplane new` prints the new id; then `mainplane message <new id> "You are continuing session <old id>. Retrieve key context from it as you go.
reply with the word moved"`. Within about 5 seconds `scripts/thread.sh <thread id>` shows `-# continued in session <new id>`, then `moved`.
- **State.** `jq '.threads["<thread id>"]'` on the state file is `<new id>`.
- **Discord goes on.** In the thread, send `reply with the word here`. `mainplane tail <new id>` shows it `via=discord`.

## Gotchas

- A session is checked once it has its first message; one made but never messaged is not.
- Only sessions made in the last day, among the newest 20 updated, are checked.
- The thread's name keeps the first session's id; the state file says which session it shows.
