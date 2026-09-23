#!/usr/bin/env bash
# daily.sh: the once-a-day refresh, run by launchd (make daily-install).
#
# Order matters. Prices come first so repricing uses today's close; labels
# before derivation so service and deposit detection see the fresh snapshot;
# repricing rebuilds the edges that derivation reads.
#
# Every step runs even if an earlier one fails, because they are largely
# independent and a failed price fetch should not also cost a sanctions
# update. The run as a whole fails if any step does, and says which.

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

LOG_DIR="$ROOT/.data/logs"
LOCK_DIR="$ROOT/.data/daily.lock"
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/daily-$(date +%F).log"

log() { printf '%s %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }

notify() {
	# A failure nobody sees is the failure mode this project exists to avoid.
	osascript -e "display notification \"$1\" with title \"tether-risk daily\"" >/dev/null 2>&1 || true
}

# mkdir is atomic, so two overlapping runs cannot both take the lock.
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
	log "another daily run holds $LOCK_DIR; exiting"
	exit 0
fi
trap 'rmdir "$LOCK_DIR" 2>/dev/null' EXIT

log "=== daily run started"

if ! docker info >/dev/null 2>&1; then
	log "FAIL docker is not running; nothing can run without the stack"
	notify "Failed: Docker is not running"
	exit 1
fi
if ! docker compose up -d --wait >>"$LOG" 2>&1; then
	log "FAIL the ClickHouse/PostgreSQL stack did not become healthy"
	notify "Failed: database stack is not healthy"
	exit 1
fi

# One build for every step, so a run uses a single consistent version.
if ! go build -o bin/ ./cmd/... >>"$LOG" 2>&1; then
	log "FAIL build"
	notify "Failed: build"
	exit 1
fi

failed=()
step() {
	local name="$1"
	shift
	local started=$SECONDS
	log "--- $name"
	if "$@" >>"$LOG" 2>&1; then
		log "ok   $name ($((SECONDS - started))s)"
	else
		log "FAIL $name ($((SECONDS - started))s)"
		failed+=("$name")
	fi
}

# Daily closes from each chain's own DEX pool (D41), continuing from the last
# day read.
step "price load TRX"         bin/price load TRX
[ -n "${ETH_RPC_URL:-}" ] && step "price load ETH" bin/price load ETH
[ -n "${BSC_RPC_URL:-}" ] && step "price load BNB" bin/price load BNB
step "labeler ingest"         bin/labeler ingest
step "price backfill"         bin/price -chain tron backfill
step "labeler derive-services" bin/labeler -chain tron derive-services
# Who created each service wallet: only new ones cost a call (D31).
step "labeler activations"    bin/labeler -chain tron activations
step "labeler derive"         bin/labeler -chain tron derive
# Edges that disagree with their transfers are recomputed (D37).
step "ingest audit-edges"     bin/ingest -chain tron audit-edges

# Keep a month of logs.
find "$LOG_DIR" -name 'daily-*.log' -mtime +30 -delete 2>/dev/null

if [ ${#failed[@]} -gt 0 ]; then
	log "=== daily run FAILED: ${failed[*]}"
	notify "Failed: ${failed[*]}. See .data/logs/daily-$(date +%F).log"
	exit 1
fi
log "=== daily run finished"
