# Plan: rename databases to `auth.db` + `silo.db`, auto-adopt legacy files, strip MySQL

Date: 2026-08-16
Status: **superseded** — see "What shipped instead" below.

## Why it was superseded

The plan kept two databases and renamed them. Keeping two was the part worth
questioning: `ccnet.db` and `seafile.db` were two files because upstream ran
two server processes (`ccnet-server`, `seaf-server`). Silo runs one. The split
bought nothing and cost a share-permission check that spanned two handles and
could never join across them.

## What shipped instead

- **One database**, `<data-dir>/silo.db`, holding both schemas. No table name
  was shared between the old pair, so the merge is a concatenation.
- **MySQL stripped entirely**, as Phase 2 proposed: driver, DSN builder,
  `[database]` config parsing, `DBEngine`, and the engine-switch SQL helpers
  (`SharedLockSuffix` was deleted outright and its comment moved to its one
  call site in `sync_api.go`).
- **No adoption code.** The plan's checkpoint-then-rename machinery assumed a
  rename; a merge is more than a rename, and Silo has no installs that are not
  the author's. `checkLegacyDatabases` in `server.go` refuses to start on a
  directory holding the old pair and prints the two `sqlite3` commands that
  fold them together, rather than silently creating an empty database beside
  real data. The recipe is in `docs/backup.md`.
- **`SEAFILE_MYSQL_DB_GROUP_TABLE_NAME`** — the one `SEAFILE_MYSQL_` variable
  that survived a MySQL strip, and the open question in Phase 2 — is now
  `SILO_GROUP_TABLE_NAME`, with the old name still accepted.
- **Phase 0's correction landed.** `docs/backup.md` claimed a naive
  `rsync -a` copies `storage/` before the database. It does not: `silo.db`
  sorts before `storage/`, as `seafile.db` did. The paragraph now says not to
  depend on traversal order at all, since the rsync is unsafe either way.

Identifier renames from Phase 3 landed with the change rather than separately:
`seafilePair` → `siloPair`, `CreateCcnetTables` + `CreateSeafileTables` →
`CreateSiloTables`, `MigrateSeafileTables` → `MigrateSiloTables`,
`ccnetSchema` + `seafileSchema` → `siloSchema`, and `share.ccnetDB` +
`share.seafileDB` → a single `share.db`. The Seafile *object-format* names
(`seafileCrypt`, `fsmgr.GetSeafile`, `seafileDataDir`) were left alone: they
name the file format and the wire protocol, not the database.
