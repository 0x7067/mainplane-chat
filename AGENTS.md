# mainplane-chat

This repo builds the bot; where it runs is each deployment's business, described on its host in `~/.config/mainplane-chat/deploy.sh` and kept with that host's setup, not here. To verify a running deployment, follow `.agents/skills/verify-mainplane-chat/SKILL.md`; build to `$MPC_BIN` with `go build -o`, never `go install`, which follows `GOBIN`.

- A change is done when you have seen it behave: run the candidate build with `-console` against the harness (README, "Trying a build") and check its output against what was asked. Compiling and passing tests is not that.
- `status()` in each adapter runs every 3 s per working thread: keep it free of harness or network calls; compute elsewhere and cache.
- Harness facts live in its source (`go list -m -f '{{.Dir}}' github.com/mainplane-ai/mainplane`): session statuses are `statefile.Status*`; a job is running while its `run` call (background true) has a `started job` result and no later `via job` message keyed by that call id, and no stop since (`harness/session.go`, `lostJobs`).
- In Slack, a message's `via` is the connector; the person is the `[name <@id>]` prefix the bridge puts on the text.
