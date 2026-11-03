# mainplane-chat

To deploy, test or roll back the running bot, follow `.agents/skills/verify-mainplane-chat/SKILL.md`. The live bot is the user LaunchAgent `local.mainplane-chat` on the M4 Mac, running `~/.local/bin/mainplane-chat`; build to that path with `go build -o` as the skill says, never `go install`, which follows `GOBIN`.

- A change is done when you have seen it behave: run the candidate build with `-console` against the harness (README, "Trying a build") and check its output against what was asked. Compiling and passing tests is not that.
- `status()` in each adapter runs every 3 s per working thread: keep it free of harness or network calls; compute elsewhere and cache.
- Harness facts live in its source (`go list -m -f '{{.Dir}}' github.com/mainplane-ai/mainplane`): session statuses are `statefile.Status*`; a job is running while its `run` call (background true) has a `started job` result and no later `via job` message keyed by that call id, and no stop since (`harness/session.go`, `lostJobs`).
- In Slack, a message's `via` is the connector; the person is the `[name <@id>]` prefix the bridge puts on the text.
