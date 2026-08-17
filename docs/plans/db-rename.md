# Plan: rename databases to `auth.db` + `silo.db`, auto-adopt legacy files, strip MySQL

Date: 2026-08-16
Status: proposed (revised after review)

## Background

Silo opens two embedded SQLite databases on startup:

| Current name | Contents | Better name |
|---|---|---|
| `ccnet.db` | users / groups (auth lives in `EmailUser`) | `auth.db` |
| `seafile.db` | repos, shares, tokens, permissions, quotas | `silo.db` |

The names are inherited wholesale from upstream Seafile (`ccnet-server`, `seaf-server`).
There is no `ccnet` process in Silo — the names are historical baggage, and the goal
is to brand the on-disk names for new installations.

Both databases run in WAL mode, so a committed transaction may live in `.db-wal`
rather than `.db`. Any rename has to account for that: moving `.db` alone silently
drops everything since the last checkpoint.

MySQL is a second database engine path (driver, DSN builder, `[database]` config parsing,
and engine-switch SQL helpers). It will never be used — strip it entirely so the SQLite-only
path can be simplified.

Existing installs are almost certainly none, but an upgrade must never orphan data:
if the old files are present, adopt them (once, at startup) rather than create fresh empty
databases next to them.

## Decisions

- New names: `auth.db` + `silo.db`.
- Existing installs: adopt old files at startup — **checkpoint, then rename the single
  `.db`** (see Phase 1; not a three-file move).
- Adoption failure is **fatal**. Starting fresh next to a user's real data looks
  exactly like total data loss.
- MySQL: removed entirely (driver, config, DSN builder, engine-switch SQL helpers).
  No env vars, no `[database]` section parsing, no `DBEngine`.
- Go identifiers renamed to match (`ccnetPair` → `authPair`, `seafilePair` → `siloPair`, …).
- User-visible change: anyone with a restore script, monitoring check or `sqlite3`
  alias pointing at the old names is affected. Goes in the next release's
  breaking-changes section, alongside the `v0.3.25` bind-address and uid changes.

Locations below are named by function or symbol rather than line number. The tree moves
(the `/simplify` pass has already shifted several), and a stale line number is worse than
no line number.

## Phase 0 — Prerequisite: fix a wrong claim in `docs/backup.md`

Independent of this plan, and worth landing first because Phase 4 would otherwise
propagate it.

`docs/backup.md` currently says:

> This is the mistake a plain `rsync -a <data-dir>/ backup/` makes: rsync walks
> alphabetically, so `storage/` is copied before `seafile.db`.

That is backwards. Sorted, the data directory reads:

```
auth.db  ccnet.db  httptemp  seafile.db  silo.db  storage  tmpfiles
```

`seafile.db` sorts *before* `storage` (`e` < `t`), so a naive rsync already transfers
the databases first — the safe order — and the same holds after the rename. The
ordering *rule* stands and must stay: it is not something to depend on a particular
tool's traversal order for, and copying a live WAL database is unsafe whatever the
order. Only the rsync example is wrong. Rewrite that paragraph; do not carry it
through the rename.

## Phase 1 — New filenames + adopt legacy files

**`fileserver/server.go`**

- `loadSQLiteDatabases()`: open `auth.db` and `silo.db`.
- Add `adoptLegacyDB(dir, oldName, newName string) error`, called for each pair before
  opening:
  1. If the new file exists → return. If the old one also exists, **log a warning**
     naming it: it holds data nobody will look at again unless told.
  2. If the old `.db` does not exist → return (fresh install).
  3. Otherwise: open the old database, `PRAGMA wal_checkpoint(TRUNCATE)`, close it,
     `os.Rename` the `.db` to the new name, then remove any leftover `-wal` / `-shm`.
  4. Any failure is returned and is fatal to startup.

  **Why checkpoint-then-rename, and not a three-file move.** Renaming `.db`, `.db-wal`
  and `.db-shm` separately has a window where the set is inconsistent: crash after the
  `.db` rename and before the `-wal`, and the new database has no WAL — every
  transaction since the last checkpoint is gone, silently. Crash the other way round and
  the old database loses its WAL instead. After a `TRUNCATE` checkpoint the `.db` is
  self-contained, so a single atomic `os.Rename` carries the whole database and every
  crash point is recoverable: before it, the old pair is intact and the next boot
  retries; after it, the new file is complete. `-shm` is scratch state rebuilt from the
  WAL — delete it, never carry it across.

- Two processes adopting at once (server plus a cron'd `gc`) is benign: `os.Rename` is
  atomic and the loser sees the new file already present and returns at step 1. Worth a
  comment so it is not re-derived.

- Tests in `server_test.go`:
  - fresh dir → `auth.db` / `silo.db` created, no warning;
  - legacy dir with un-checkpointed data in `-wal` → adopted, **and the un-checkpointed
    rows are readable afterwards** (the point of the checkpoint step);
  - legacy `-wal` / `-shm` left behind are cleaned up;
  - both old and new present → new wins, old untouched, warning logged;
  - only one of the two pairs is legacy → each adopts independently;
  - adoption is idempotent across two runs;
  - unwritable directory → startup fails rather than creating empty databases.

**`fileserver/backup.go`**

`RunBackupDB` stats the database paths itself; it does not go through `openStores()`,
so it will not pick up adoption for free. Left as a plain rename of the name list, the
new binary run against a not-yet-adopted directory fails with "cannot read auth.db" —
and "back up before upgrading" is the most likely first command anyone runs.

Read whichever pair exists (new names first, legacy as fallback) and **always write the
new names into the destination**. Deliberately no adoption here: a backup must not
mutate the directory it is reading. A backup taken from a legacy install therefore
restores straight into the new layout.

`gc` and `token` need no change — both go through `openStores()` → `loadDatabases()`.

## Phase 2 — Strip MySQL

- **`go.mod` / `go.sum`**: drop `github.com/go-sql-driver/mysql`.
- **`fileserver/dbutil/dbutil.go`**:
  - Remove `OpenMySQL`, the `_ "github.com/go-sql-driver/mysql"` import,
    `EngineMySQL` / `EnginePostgres` / `DBEngine`.
  - Collapse `InsertOrReplace`, `InsertOrIgnore`, `SharedLockSuffix` and
    `makePlaceholders` to SQLite-only (this also removes the dead Postgres branches).
    `SharedLockSuffix` becomes a constant empty string — consider whether it earns its
    existence at all once there is one engine, or whether the call site should just drop
    it with the comment moved there.
- **`fileserver/dbutil/schema.go`**: remove the `DBEngine == EngineMySQL` branch in
  `addIndexIfMissing`.
- **`fileserver/option/option.go`**:
  - Delete the `DBOption` MySQL fields (`User`, `Password`, `Host`, `Port`, `CcnetDbName`,
    `SeafileDbName`, `CaPath`, `UseTLS`, `SkipVerify`, `Charset`, `DBEngine`).
  - Delete `loadDBOptionFromEnv`, the `[database]`-section parsing in
    `loadDBOptionFromFile`, and the `SEAFILE_DB_TYPE` check in `LoadDBOption`.
  - Delete the `DBType` global.
  - Remove `LoadDBOption` and its callers (`loadDatabases`, `RunBackupDB`). A stray
    `[database]` section in a user's `seafile.conf` is simply ignored.
  - **Decide `SEAFILE_MYSQL_DB_GROUP_TABLE_NAME`** (`GroupTableName`). Despite the name
    it is read on the SQLite path too and feeds `share.Init`, so it is not MySQL-only
    and does not fall out with the rest. Keep it, rename it, or drop it — but decide,
    or it is the one `SEAFILE_MYSQL_` left standing after a MySQL strip.
- **`fileserver/server.go`**: delete `loadMySQLDatabases`, `buildMySQLDSN` and
  `registerCA` (which uses `mysql.RegisterTLSConfig`); `loadDatabases` calls
  `loadSQLiteDatabases()` unconditionally; `checkpointAndClose` always checkpoints.
- **Tests**:
  - `dbutil_test.go`: remove the MySQL/Postgres engine tests (`TestInsertOrReplaceMySQL`,
    `TestInsertOrReplacePostgres*`, `TestInsertOrIgnoreMySQL`,
    `TestInsertOrIgnorePostgres`) and the MySQL read==write `Close` test.
  - `repomgr_test.go`: remove the real-MySQL integration test. Note its `TestMain`
    currently `os.Exit(0)`s unless `TEST_REPO_ID` is set, so the whole package is
    skipped today — removing it should either give the package a working SQLite
    harness or leave it honestly empty, not leave a skip that looks like coverage.
  - Remove all `dbutil.DBEngine = …` stubs (`gc_test.go`, `apitokenstore_test.go`,
    `api_test.go`, `schema_test.go`, `sync_api_test.go`).

## Phase 3 — Rename Go identifiers

- DB pair globals: `ccnetPair` → `authPair`, `seafilePair` → `siloPair`
  (server.go, sync_api.go, gc.go, fileop.go, quota.go, size_sched.go, token_cmd.go,
  backup.go + tests).
- Schema functions: `CreateCcnetTables` → `CreateAuthTables`,
  `CreateSeafileTables` → `CreateSiloTables`, `MigrateSeafileTables` → `MigrateSiloTables`,
  `ccnetSchema` → `authSchema`, `seafileSchema` → `siloSchema` (dbutil/schema.go).
- Sub-package DB handles: repomgr `seafileDB` / `seafileWriteDB` → `siloDB` /
  `siloWriteDB`; share `ccnetDB` → `authDB`; authmgr `ccnetReadDB` / `ccnetWriteDB` →
  `authReadDB` / `authWriteDB`; api `seafileDB` → `siloDB`.
- **Do NOT rename** the Seafile *object-format* names — `seafileCrypt`, `fsmgr.GetSeafile` /
  `NewSeafile`, `seafileDataDir` — those refer to the file format and the wire protocol,
  not the database.
- Leave legacy C (`common/`, `server/`, `fuse/`) untouched (not built).
- Land as **one commit containing nothing but the rename**, so the diff is provably
  mechanical.

## Phase 4 — Docs

- `README.md`: the backup section, plus **line 34** ("Two logical databases — `ccnet`
  (users, groups) and `seafile` (repos, shares, tokens)") and **line 130** ("it creates
  the ccnet and seafile SQLite databases"). All three, not just the backup one.
- `docs/backup.md` — the table, "Why `cp seafile.db` is not a backup", the
  `sqlite3 … integrity_check` example. (The rsync paragraph is Phase 0.)
- `docs/encryption.md` — the `sqlite3 /tmp/silo-data/seafile.db …` example.
- `cmd/silo/main.go` — the `silo backup-db` help text names both databases.
- `docs/notes.md` and `docs/plan.md` — conceptual "ccnet/seafile DB" → new names
  (note in plan.md that the Seafile-format *object* names remain).
- `.gitignore` (`tests/conf/ccnet.db`) stays — legacy C test suite.
- Release notes: new section listing the old → new filenames, stating that adoption is
  automatic on first start, and that any external tooling naming the files must be
  updated.

## Sequencing

- **Phase 0** first, on its own — it is a correction to shipped docs, not part of this
  work.
- **Phases 1 and 2** are independent of each other; either order.
- **Phase 3 last**, and only once the in-flight `/simplify` has landed. It is a
  mechanical rename across eight-plus files that the simplify is already editing
  (`server.go`, `sync_api.go`, `option.go`, `gc.go`), and resolving a rename against an
  in-flight refactor is how the wrong side wins a conflict.

## Verification

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- Manual smoke:
  - fresh `-d` dir → server creates `auth.db` / `silo.db`;
  - a directory pre-seeded with `ccnet.db` / `seafile.db` **with data still in the WAL**
    (write, then kill the server rather than stopping it) → adopted at first boot with a
    log line, and the un-checkpointed rows are still present;
  - `silo backup-db` against a legacy directory → succeeds, writes `auth.db` / `silo.db`
    into the destination, leaves the source untouched;
  - restore that backup into a fresh data directory → server starts against it clean;
  - second start → no adoption, no warning, no churn.
