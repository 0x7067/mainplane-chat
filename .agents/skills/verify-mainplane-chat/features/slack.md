# Slack

The Slack side has no tester bot: tests post as the user in his DM with the app (`SLACK_DM` in `tester.env`) with `scripts/slack-say.sh`, with the user's approval. Read with `scripts/slack-thread.sh`. State keys are `slack:<SLACK_DM>` (seen) and `slack:<SLACK_DM>/<thread ts>`.

Slack apps differ by deployment: their scopes, who they hear, and whether a test DM or channel exists. Start with `scripts/slack-config.sh` (read-only). It names the app, the `slack-manifest.yaml` scopes its token lacks, and for each group of recipes below `run`, `skip: <why>` or `degraded: <what to expect instead>`. Drive the `run` and `degraded` ones as it says, and report each skip with its reason. A degraded result that matches is a pass for that configuration, not of the full feature.

- **Start.** `slack-say.sh "verify slack <run>: reply with the word ok"`. Expect a reply in the message's thread and ✅ on the message; the state has the thread.
- **Framing.** `mainplane tail <id>` of that session: with every scope, its first message opens `(Note from the Slack bridge: you are <app>, an app in Slack. This conversation is a Slack thread in a direct message with you. ...)`, then `[<name> <@<user id>>]: <text>`. A later reply in the thread has only the `[<name> <@<user id>>]: ` line; a thread command such as `!info` has none. Without `users:read`, `<name>` is the user id; without `im:read`, the note says `a Slack conversation`; the log has the `users.info` or `conversations.info` `missing_scope` lines.
- **Channels.** In a channel the app is in (`SLACK_CH=<channel id>` before `slack-say.sh` and `slack-thread.sh`; the app's user id is `.user_id` of `auth.test`): a message without `<@<app id>>` gets no reaction and no session. `<@<app id>> reply with the word ok` starts a session in its thread; the first message's note says `the channel #<name>`. In that thread, a reply without the mention is ignored; one with it continues the session.
- **Turn status.** A reply that makes tool calls: a plan block status message, `Working · N calls`, with a task per call (`run on <worker>`, its command in code style) going `in_progress` to `complete`, or `error` on a non-zero exit. At the end its title is `Done · N calls · Ts · P% context · <id>`; `Failed after` or `Stopped after` with open tasks `error`; `Continued after` when the session was full, with open tasks `pending` and no ❌. Read it with `conversations.replies` and jq on `.blocks[0]`: `slack-thread.sh` shows only the fallback text.
- **Commands.** `!info`, `!continue <msg>` (a "continuing in session" note; the thread moves), `!model` (lists the models), `!delete` (the session is gone and the agent session closed).
- **Quote.** `mainplane message <id> "<text>"` shows as `> **cli:** <text>`.
- **Catch-up.** Stop the service, reply in the thread, start it: the reply is answered.
- **Gone.** `mainplane delete <id>`: the thread's entry leaves the state, and the agent session is closed.
- **Stop.** Slack's own Stop button: only the user can press it. Ask him.

Code-only checks, as the tests cannot post as another person: `*` in `MAINPLANE_SLACK_USERS` lets in every full member and no guest or bot (`TestFullMember`), and needs `users:read` for anyone not listed (`TestAllowedListedUsersWithoutLookup`); the agent session's initiator is the message's author (`InitiatorUserID` in `slack.go`).
