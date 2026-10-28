# Restore drill: first run (2026-08-11)

> **Record, kept as written.** This was the Background section of
> [Back up Hardcount](../../ops/backup-considerations.md), moved here on 2026-10-22.

The first restore drill, on 2026-08-11, failed and found four defects in the scripts. All four were fixed the same day, and the drill then passed.

| Date | Result | Notes |
|---|---|---|
| 2026-08-11 | **FAIL → 4 defects → PASS** (7s total: backup 1s, restore 3s; 5 critical tables count-matched) | First-ever run. Found: (1) host `pg_dump` 18 vs server 16 → unrestorable archive; (2) host `pg_restore` 18 vs target 16 → client `SET transaction_timeout` rolled restore back; (3) fresh-cluster restore rolled back on missing `hardcount_app` role — the DR path had never worked; (4) restore.sh's validation query referenced a nonexistent column (`table_name` vs `relname`) — broken since birth. All four fixed same day; drill re-run green. |
