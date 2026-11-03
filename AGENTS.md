# mainplane-chat

To deploy, test or roll back the running bot, follow `.agents/skills/verify-mainplane-chat/SKILL.md`. Two bots run this code: the test bot on admin (systemd user unit, `~/go/bin/mainplane-chat`), where verification runs, and the live bot on the M4 Mac (user LaunchAgent `local.mainplane-chat`, `~/.local/bin/mainplane-chat`). Build to those paths with `go build -o` as the skill says, never `go install`, which follows `GOBIN`.
