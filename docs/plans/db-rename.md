# Plan: rename databases to `auth.db` + `silo.db`, auto-adopt legacy files, strip MySQL

Date: 2026-08-16
Status: **superseded** — see "What shipped instead" below.

## Why it was superseded

The plan kept two databases and renamed them. Keeping two was the part worth
questioning: the inherited pair were two files because upstream ran two server
processes, one for identity and one for libraries. Silo runs one. The split
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
  the author's. `checkLegacyDatabases` in `server.go` refused to start on a
  directory holding the old pair and printed the two `sqlite3` commands that
  fold them together, rather than silently creating an empty database beside
  real data. That check has since been deleted (`d644f8f`) — see
  `docs/backup.md`.
- **The group-table-name environment variable** — the one upstream-prefixed
  variable that survived a MySQL strip, and the open question in Phase 2 — is
  now `SILO_GROUP_TABLE_NAME` and nothing else; the old name was accepted for
  one release and has since been dropped.
- **Phase 0's correction landed.** `docs/backup.md` claimed a naive
  `rsync -a` copies `storage/` before the database. It does not: `silo.db`
  sorts before `storage/`, as the file it replaced did. The paragraph now says
  not to depend on traversal order at all, since the rsync is unsafe either way.

Identifier renames from Phase 3 landed with the change rather than separately:
the two inherited connection pairs, table-creation and migration functions and
schema constants all collapsed to one `silo`-prefixed set, and the two share
handles to a single `share.db`. The inherited *object-format* identifiers were
left alone at the time, because they named the file format and the wire
protocol rather than the database; both were replaced outright in `5d4baa0` and
none of those names survive.
