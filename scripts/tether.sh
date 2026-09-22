#!/usr/bin/env bash
# tether.sh: refresh Tether's USDT blacklist every few minutes, run by launchd
# (make tether-install). Tether freezes in clusters and a frozen wallet's
# counterparty is at risk mostly within three days, so the daily run is too
# slow to warn anyone (docs/DECISIONS.md D34).

set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

if [ -f .env ]; then
	set -a
	. ./.env
	set +a
fi

docker info >/dev/null 2>&1 || exit 1
go build -o bin/ ./cmd/labeler || exit 1
exec bin/labeler tether
