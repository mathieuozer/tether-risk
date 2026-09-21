#!/usr/bin/env bash
# worker.sh: the long-running ingest worker, kept alive by launchd
# (make worker-install). Screening queues counterparties; this drains them.
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

go build -o bin/ ./cmd/ingest || exit 1
exec bin/ingest -chain tron -workers 1 worker
