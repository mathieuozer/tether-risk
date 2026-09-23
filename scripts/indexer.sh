#!/usr/bin/env bash
# indexer.sh tail|serve: the TRON index (docs/INDEXER_PLAN.md), kept alive by
# launchd (make indexer-install).
#
#   tail   follows the solidified head into the tron_index database from
#          INDEXER_SOURCE (default alchemy; a local node's URL once there is
#          one). It resumes from its cursor, so a restart loses nothing.
#   serve  answers index queries on INDEX_API_ADDR (default 127.0.0.1:8098).
#
# Exits when the stack is unreachable; launchd restarts it after
# ThrottleInterval.

set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

if [ -f .env ]; then
	set -a
	. ./.env
	set +a
fi

MODE="${1:-}"
case "$MODE" in
tail | serve) ;;
*)
	echo "usage: indexer.sh tail|serve" >&2
	exit 2
	;;
esac

if ! docker info >/dev/null 2>&1; then
	echo "$(date '+%F %T') docker is not running; exiting for launchd to retry"
	exit 1
fi
docker compose up -d --wait >/dev/null 2>&1 || exit 1

go build -o bin/ ./cmd/indexer || exit 1
echo "$(date '+%F %T') starting indexer $MODE"
if [ "$MODE" = tail ]; then
	exec bin/indexer -source "${INDEXER_SOURCE:-alchemy}" tail
fi
exec bin/indexer -addr "${INDEX_API_ADDR:-127.0.0.1:8098}" serve
