#!/usr/bin/env bash
# Copy everything this project keeps outside git to a folder (an external
# disk), so scripts/move-restore.sh can rebuild the same state on another
# computer. Usage: scripts/move-backup.sh /Volumes/DISK/tether-risk-move
#
# Saved: the PostgreSQL database (pg_dump), every ClickHouse table of
# tether_risk (schema + data, views and materialized views last), .env and
# .data without logs. Not saved: tron_index, the 3-day local index (D45),
# which the indexer refills from Alchemy on the new computer.
set -euo pipefail

DEST=${1:?usage: $0 <destination folder>}
cd "$(dirname "$0")/.."
command -v zstd >/dev/null || { echo "zstd missing: brew install zstd"; exit 1; }
docker compose ps --status running --services | grep -q clickhouse || { echo "stack not running: make up"; exit 1; }

mkdir -p "$DEST"/clickhouse/{ddl,data} "$DEST"/files
CH="docker exec -i tr-clickhouse clickhouse-client --database tether_risk"
DB=tether_risk
# Every loop reads its list from fd 3: `docker exec -i` would otherwise
# swallow the rest of the list from stdin and the loop would stop after one.

echo "== PostgreSQL"
docker exec tr-postgres pg_dump -U tether -d "$DB" -Fc -Z 6 >"$DEST/postgres.dump"
ls -lh "$DEST/postgres.dump"

echo "== ClickHouse schema"
# Tables first (they hold the data), then views and materialized views,
# which are created only after the data is back so nothing is inserted twice.
$CH -q "SELECT name FROM system.tables WHERE database='$DB' AND engine NOT IN ('View','MaterializedView','Memory') ORDER BY name" >"$DEST/clickhouse/tables.txt"
$CH -q "SELECT name FROM system.tables WHERE database='$DB' AND engine IN ('View','MaterializedView') ORDER BY engine='MaterializedView', name" >"$DEST/clickhouse/views.txt"
while read -r t <&3; do
  $CH -q "SHOW CREATE TABLE $DB.\`$t\` FORMAT TSVRaw" </dev/null >"$DEST/clickhouse/ddl/$t.sql"
done 3< <(cat "$DEST/clickhouse/tables.txt" "$DEST/clickhouse/views.txt")

echo "== ClickHouse data"
: >"$DEST/clickhouse/counts.tsv"
while read -r t <&3; do
  n=$($CH -q "SELECT count() FROM $DB.\`$t\`" </dev/null)
  printf '%s\t%s\n' "$t" "$n" >>"$DEST/clickhouse/counts.tsv"
  echo "  $t: $n rows"
  $CH -q "SELECT * FROM $DB.\`$t\` FORMAT Native" </dev/null | zstd -q -T0 -3 >"$DEST/clickhouse/data/$t.native.zst"
done 3<"$DEST/clickhouse/tables.txt"

echo "== Files"
[ -f .env ] && cp .env "$DEST/files/env"
rsync -a --exclude logs --exclude '*.lock' .data/ "$DEST/files/data/"

echo "== Verify"
docker exec -i tr-postgres pg_restore --list <"$DEST/postgres.dump" >/dev/null && echo "  postgres.dump readable"
want=$(wc -l <"$DEST/clickhouse/tables.txt" | tr -d ' ')
got=$(ls "$DEST"/clickhouse/data/*.zst | wc -l | tr -d ' ')
[ "$want" = "$got" ] || { echo "  only $got of $want ClickHouse tables exported"; exit 1; }
for f in "$DEST"/clickhouse/data/*.zst; do zstd -q -t "$f"; done && echo "  clickhouse: all $got tables exported, files intact"
git rev-parse HEAD >"$DEST/git-commit.txt"
du -sh "$DEST"
echo "done: $DEST"
