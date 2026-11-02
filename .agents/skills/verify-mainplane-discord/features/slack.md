# Slack

The Slack side has no tester bot: tests post as the user in his DM with the app (`D0CE46P2QJY`; user `U09BMR1UGQJ`, bot `U0CDV71FRTP`) with `scripts/slack-say.sh`, which he approved. Read with `scripts/slack-thread.sh`. State keys are `slack:D0CE46P2QJY` (seen) and `slack:D0CE46P2QJY/<thread ts>`.

- **Start.** `slack-say.sh "verify slack <run>: reply with the word ok"`. Expect a reply in the message's thread and ✅ on the message; the state has the thread.
- **Turn status.** A reply that makes tool calls: a context-block status message, shrunk at the end to `` `<id>` · N calls · Ts · P% context ``.
- **Commands.** `!info`, `!continue <msg>` (a "continuing in session" note; the thread moves), `!model` (lists the models).
- **Quote.** `mainplane message <id> "<text>"` shows as `> **cli:** <text>`.
- **Catch-up.** Stop the service, reply in the thread, start it: the reply is answered.
- **Gone.** `mainplane delete <id>`: the thread's entry leaves the state, and the agent session is closed.
- **Stop.** Slack's own Stop button: only the user can press it. Ask him.
