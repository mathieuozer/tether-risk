#!/usr/bin/env bash
# maintenance-reprice.sh: revalue every stored TRX transfer from the
# on-chain closes and rebuild the edges (docs/DECISIONS.md D41). The rewrite
# is summed twice by the edge views until the rebuild, and the rebuild empties
# the edge tables while it runs, so the API, the worker and the blacklist
# refresh are stopped for the duration and started again at the end.

set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
if [ -f .env ]; then set -a; . ./.env; set +a; fi

U="gui/$(id -u)"
SERVICES="com.tether-risk.api com.tether-risk.worker com.tether-risk.tether"
LOG=".data/logs/maintenance-$(date +%F-%H%M).log"
mkdir -p .data/logs
log() { printf '%s %s\n' "$(date '+%F %T')" "$*" | tee -a "$LOG"; }

go build -o bin/ ./cmd/price ./cmd/ingest || exit 1

log "stopping writers"
for s in $SERVICES; do launchctl bootout "$U/$s" 2>/dev/null; done
sleep 5
pkill -f "bin/ingest -chain" 2>/dev/null

status=0
log "reprice TRX"
bin/price -chain tron reprice-all TRX >>"$LOG" 2>&1 || status=1
if [ $status -eq 0 ]; then
	log "rebuild edges"
	bin/ingest -chain tron rebuild-edges >>"$LOG" 2>&1 || status=1
fi
if [ $status -eq 0 ]; then
	log "audit edges"
	bin/ingest -chain tron audit-edges >>"$LOG" 2>&1 || status=1
fi

log "starting writers"
for s in $SERVICES; do launchctl bootstrap "$U" "$HOME/Library/LaunchAgents/$s.plist" 2>/dev/null; done
[ $status -eq 0 ] && log "done" || log "FAILED; see $LOG"
exit $status
