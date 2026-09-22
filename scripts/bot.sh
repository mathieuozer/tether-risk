#!/usr/bin/env bash
# bot.sh: the Telegram bot, kept alive by launchd (make bot-install).
#
# Needs TELEGRAM_BOT_TOKEN, TELEGRAM_ADMIN_IDS and BILLING_SUPPORT_CONTACT in
# .env, and optionally BILLING_USDT_ADDRESS (docs/DECISIONS.md D26).
#
# Exits when the stack is unreachable. launchd restarts it after
# ThrottleInterval, so it recovers on its own once Docker is back.

set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

# TRONGRID_API_KEY and similar live in the gitignored .env.
if [ -f .env ]; then
	set -a
	. ./.env
	set +a
fi

if ! docker info >/dev/null 2>&1; then
	echo "$(date '+%F %T') docker is not running; exiting for launchd to retry"
	exit 1
fi
docker compose up -d --wait >/dev/null 2>&1 || exit 1

ADDR="${API_ADDR:-127.0.0.1:8099}"
# ":8099" listens on every interface; the bot reaches it on loopback.
case "$ADDR" in :*) ADDR="127.0.0.1$ADDR" ;; esac
API="http://$ADDR"

go build -o bin/ ./cmd/bot || exit 1
echo "$(date '+%F %T') starting bot against $API"
exec bin/bot -api "$API"
