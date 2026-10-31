#!/usr/bin/env bash
# Rebuild the state saved by scripts/move-backup.sh on a fresh computer.
# Usage: make up && scripts/move-restore.sh /Volumes/DISK/tether-risk-move
# Run it before `make migrate`; the restored schema_migrations then makes
# migrate a no-op.
set -euo pipefail

SRC=${1:?usage: $0 <backup folder>}
cd "$(dirname "$0")/.."
command -v zstd >/dev/null || { echo "zstd missing: brew install zstd"; exit 1; }
docker compose ps --status running --services | grep -q clickhouse || { echo "stack not running: make up"; exit 1; }

CH="docker exec -i tr-clickhouse clickhouse-client --database tether_risk"
DB=tether_risk

[ "$($CH -q "SELECT count() FROM system.tables WHERE database='$DB'")" = 0 ] ||
  { echo "ClickHouse $DB already has tables; restore needs an empty stack (make clean && make up)"; exit 1; }
[ "$(docker exec tr-postgres psql -U tether -d $DB -Atc "select count(*) from pg_tables where schemaname='public'")" = 0 ] ||
  { echo "PostgreSQL $DB already has tables; restore needs an empty stack (make clean && make up)"; exit 1; }

if [ -f "$SRC/files/env" ] && [ ! -f .env ]; then cp "$SRC/files/env" .env && chmod 600 .env && echo "== .env restored"; fi
mkdir -p .data && rsync -a --exclude "._*" "$SRC/files/data/" .data/

echo "== PostgreSQL"
docker exec -i tr-postgres pg_restore -U tether -d "$DB" --no-owner --exit-on-error <"$SRC/postgres.dump"

echo "== ClickHouse tables"
# Lists are read from fd 3 so `docker exec -i` cannot swallow them from stdin.
while read -r t <&3; do $CH --multiquery <"$SRC/clickhouse/ddl/$t.sql"; done 3<"$SRC/clickhouse/tables.txt"
while read -r t <&3; do
  echo "  $t"
  zstd -q -d -c "$SRC/clickhouse/data/$t.native.zst" | $CH -q "INSERT INTO $DB.\`$t\` FORMAT Native"
done 3<"$SRC/clickhouse/tables.txt"
echo "== ClickHouse views"
while read -r t <&3; do $CH --multiquery <"$SRC/clickhouse/ddl/$t.sql"; done 3<"$SRC/clickhouse/views.txt"

echo "== Verify row counts"
bad=0
while IFS=$'\t' read -r t want <&3; do
  got=$($CH -q "SELECT count() FROM $DB.\`$t\`" </dev/null)
  if [ "$got" = "$want" ]; then echo "  ok  $t $got"; else echo "  BAD $t got $got want $want"; bad=1; fi
done 3<"$SRC/clickhouse/counts.tsv"
[ "$(wc -l <"$SRC/clickhouse/counts.tsv" | tr -d ' ')" = "$(wc -l <"$SRC/clickhouse/tables.txt" | tr -d ' ')" ] ||
  { echo "  BAD counts.tsv does not list every table"; bad=1; }
docker exec tr-postgres psql -U tether -d "$DB" -Atc \
  "select count(*) from pg_tables where schemaname='public'" | sed 's/^/  postgres tables: /'
[ $bad = 0 ] && echo "restore complete" || { echo "restore finished with mismatches"; exit 1; }
