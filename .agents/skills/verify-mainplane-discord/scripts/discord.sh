#!/usr/bin/env bash
# discord.sh <api path>: GET a Discord API path as the bot (read-only), JSON out.
# The token comes from the service's env file and is never printed.
set -euo pipefail
set -a; . "$HOME/.config/mainplane-discord/env"; set +a
curl -fsS -H "Authorization: Bot $DISCORD_TOKEN" "https://discord.com/api/v10$1"
