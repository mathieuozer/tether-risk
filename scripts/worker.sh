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

# One worker without a key: D17 measured that a pool only multiplied 429s.
# With a key the budget is 8/s, and each request is ~1 s of latency, so a
# single stream would use an eighth of it; three share it without contention.
WORKERS=1
if [ -n "${TRONGRID_API_KEY:-}" ]; then
	WORKERS=3
fi

go build -o bin/ ./cmd/ingest || exit 1

# One process per chain with a data path: a worker runs one chain's adapter
# and claims only that chain's jobs (D40). Ethereum and BSC run through
# Alchemy when their endpoints are set.
pids=()
echo "$(date '+%F %T') starting $WORKERS tron worker(s)"
bin/ingest -chain tron -workers "$WORKERS" worker &
pids+=($!)
if [ -n "${ETH_RPC_URL:-}" ]; then
	echo "$(date '+%F %T') starting an ethereum worker"
	bin/ingest -chain ethereum -workers 1 worker &
	pids+=($!)
fi
if [ -n "${BSC_RPC_URL:-}" ]; then
	echo "$(date '+%F %T') starting a bsc worker"
	bin/ingest -chain bsc -workers 1 worker &
	pids+=($!)
fi

# launchd runs this with /bin/bash 3.2, which has no `wait -n`. When any
# worker stops, stop the rest and exit, so launchd restarts them together.
trap 'kill "${pids[@]}" 2>/dev/null' TERM INT
while :; do
	for pid in "${pids[@]}"; do
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "$(date '+%F %T') worker $pid stopped; restarting all"
			kill "${pids[@]}" 2>/dev/null
			wait
			exit 1
		fi
	done
	sleep 5
done
