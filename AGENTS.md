# mainplane-chat

To deploy, test or roll back the running bot, follow `.agents/skills/verify-mainplane-chat/SKILL.md`. The live bot is the user LaunchAgent `local.mainplane-chat` on the M4 Mac, running `~/.local/bin/mainplane-chat`; build to that path with `go build -o` as the skill says, never `go install`, which follows `GOBIN`.
