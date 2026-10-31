# Setting up on a new computer (Mac mini, 2026-10-31)

On 2026-10-31 the project moved from the MacBook Air to the Mac mini. The
MacBook's disk was full, so its Docker data was deleted after the backup was
verified. The code is in git. Everything outside git is on the LaCie disk
attached to the Mac mini:

```
/Volumes/LaCie/tether-risk-move-31102026/
  postgres.dump          pg_dump -Fc of the tether_risk database
  clickhouse/            tether_risk tables: DDL, Native+zstd data, row counts
  files/env              the .env file (secrets; never commit it)
  files/data/            .data without logs (reports, screenshots, benchmark)
  git-commit.txt         the commit the backup was taken at
  SHA256SUMS             checksums of every file above
```

5.5 GB in all. Check it before restoring:
`cd /Volumes/LaCie/tether-risk-move-31102026 && grep -v '/\._' SHA256SUMS | shasum -a 256 -c --quiet`.
The `._*` files next to the real ones are macOS metadata that the LaCie's
file system needs; ignore them.

Row counts at backup time: ClickHouse transfers 34,716,670, transfers_by_to
34,221,440, edges 16,952,360, edges_by_to 16,977,350; PostgreSQL 25 tables
(run_paths is the largest, 3.5 GB on disk).

The restore script was written for this move and has not run end to end
yet: the old computer had no room to try it. If it stops, fix the script,
commit the fix and run it again on a clean stack (`make clean && make up`).

Not in the backup: `tron_index`, the 3-day local TRON index (D45). The
indexer rebuilds it from Alchemy. One of its tables had broken parts after
Docker was force-killed on a full disk, so it was not worth copying.

## Prompt for Claude Code on the new computer

Paste this into Claude Code, opened in the cloned repository:

> Read docs/MOVE.md and set this project up on this computer exactly as it
> ran on the old one. Install what is missing (Homebrew, Go at the go.mod
> version, Docker Desktop, zstd), then `make up`, then restore with
> `scripts/move-restore.sh /Volumes/LaCie/tether-risk-move-31102026` before
> `make migrate`. Check the row counts the script prints, run `make check`,
> start the services with the install targets below, and confirm a screen of
> a known address works through the API. Tell me plainly what worked and what
> did not. Rotate the Alchemy keys in .env (they were pasted in a chat on
> 2026-09-23).

## Steps (what the prompt does)

1. Install the tools: `brew install go zstd git` and Docker Desktop. Give
   Docker Desktop enough disk (Settings → Resources): about 60 GB, more if
   the TRON index will keep more than 3 days.
2. Clone the repository: `git clone git@github.com:mathieuozer/tether-risk.git`.
   The old computer used an SSH alias, `github.com-tether-risk`; set up a key
   for this computer or clone over HTTPS.
3. Start the empty stack: `make up`.
4. Restore: `scripts/move-restore.sh /Volumes/LaCie/tether-risk-move-31102026`.
   This copies `.env` and `.data`, restores PostgreSQL, creates the ClickHouse
   tables, loads them, creates the views and materialized views last, then
   compares row counts with the backup. It refuses to run on a stack that
   already has tables.
5. `make migrate`. It is a no-op, because schema_migrations came back too.
6. `make build && make check`.
7. Start the services that ran on the old computer:
   `make worker-install api-install indexer-install tether-install daily-install`.
   The bot (`make bot-install`) was never live; it still waits on the
   Telegram values in .env (see the go-live checklist).
8. The indexer refills `tron_index` from Alchemy. Leave it a few hours before
   relying on index-backed results (the blacklist and watch flows, D46).
