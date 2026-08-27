# Backup and restore

A Silo deployment is two kinds of state, and they need different handling:

| What | Where | How to copy |
|---|---|---|
| Database | `<data-dir>/silo.db` | `silo backup-db` |
| Object store | `<data-dir>/storage/` | `rsync`, `cp -a`, snapshot, tar — anything |

The object store is an immutable content-addressed file tree; any ordinary
copy tool handles it. The database is not safe to copy with `cp`, and the
order the two halves are captured in decides whether the backup restores.

## Why `cp silo.db` is not a backup

The database runs in WAL mode. A committed transaction is durable once it
reaches `silo.db-wal`; it only moves into `silo.db` at a checkpoint, and
Silo checkpoints on clean shutdown. Copying only `silo.db` off a running
server therefore silently drops every write since the last checkpoint — no
error, no warning, and nothing in the restored copy to show that anything is
missing.

Copying `silo.db`, `silo.db-wal` and `silo.db-shm` together is not a
fix either. The three files are read at three different instants, and the set
you end up with need not be a state the database was ever in.

`silo backup-db` uses SQLite's `VACUUM INTO`, which takes a read transaction
over the source and writes one consistent, already-checkpointed file. It is
safe while the server is running and blocks no writer.

## The order rule

**Database first. Object store second.**

Objects are content-addressed and never rewritten, so every object referenced
by a branch head captured at T1 is still on disk at T2. Capture in that order
and the backup is self-consistent even though the server kept running
throughout: the store copy is simply newer than the heads, containing extra
objects nothing points at yet. Those are harmless — the next `silo gc` reclaims
them if their library is deleted.

Reverse the order and the guarantee inverts. A store copied at T1 with heads
copied at T2 can contain heads referencing objects written between T1 and T2 —
objects the backup does not have. That library restores broken, and the
breakage does not show up until someone opens the file.

Do not depend on a copy tool's traversal order to get this right. `rsync -a
<data-dir>/ backup/` happens to walk alphabetically, and `silo.db` sorts before
`storage/` — but that is a coincidence of the names, not a guarantee, and it
does not make the rsync safe: it is still copying a live WAL database, which
is the mistake the section above is about.

The one thing that invalidates the rule is object deletion, so **do not run
`silo gc -delete` while a backup is in progress**.

## Live backup

Safe with the server running:

```sh
silo backup-db -d /var/lib/silo /backup/silo/$(date +%F)
rsync -a /var/lib/silo/storage/ /backup/silo/$(date +%F)/storage/
```

`backup-db` refuses to overwrite an existing destination file, so a failed run
cannot replace a good backup with a broken one. Pass `-f` when rotating into a
fixed path.

For repeated backups, `rsync --link-dest` against the previous night's copy
makes the store hardlink-deduplicated. Objects are immutable, so unchanged
files link rather than copy.

## Cold backup

With the server stopped, the databases are checkpointed on shutdown and the
whole directory can be copied as a unit:

```sh
systemctl stop silo
cp -a /var/lib/silo /backup/silo/$(date +%F)
systemctl start silo
```

The order rule does not apply here — nothing is writing.

## Restore

```sh
systemctl stop silo
rm -rf /var/lib/silo
cp -a /backup/silo/2026-08-16 /var/lib/silo
systemctl start silo
```

A restored library may be behind what a client already has locally. Clients
resolve this by uploading what the server is missing, the same way they handle
any other divergence — nothing on the server side needs resetting.

## Verifying a backup

The database is checked cheaply:

```sh
sqlite3 /backup/silo/2026-08-16/silo.db 'PRAGMA integrity_check;'
```

A snapshot from `backup-db` stands alone: there should be no `-wal` or `-shm`
file beside it. If there is, it did not come from `backup-db`.

To confirm the store matches the heads, restore into a scratch data directory
and run `silo serve` against it — a client that syncs a library end to end has
verified every object that library's head references.

## Schema version

`silo.db` carries its own schema version in SQLite's `PRAGMA user_version` —
no extra table, just the four bytes SQLite reserves in the file header for
exactly this. `fileserver/dbutil/schema.go` defines `SchemaVersion`, the
version the running binary expects.

At startup, `CreateSiloTables` reads `PRAGMA user_version` before touching
anything. Unstamped (`0`) is two different populations wearing one value — a
database this call is about to create, and every database written before the
stamp existed — and it tells them apart by whether `sqlite_master` already
holds any tables:

- **Unstamped and empty** — a genuinely fresh database. The schema is applied
  (`CREATE TABLE`/`INDEX IF NOT EXISTS`), and only once that succeeds is the
  database stamped with `SchemaVersion`.
- **Unstamped but already holding tables** — written before this check
  existed, which the library rename (`0.5.0`) already shipped against.
  Refused immediately with its own message: *"this database has tables but
  no schema version stamp, so it was written before this build's schema
  (schema version N) ... this package has no migration path."* Different
  wording from a version mismatch below, but from the operator's side it's
  the same situation: a database this build cannot be trusted to run
  against.
- **Stamped, and it matches** — the schema is (re-)applied as above; normal
  startup.
- **Stamped, and it doesn't match** — refused immediately, before any SQL
  runs, with an error naming both versions:

  ```
  database schema version 3 does not match what this build expects (schema version 4);
  refusing to start rather than run a mismatched schema against it. If this database
  is disposable, delete it and let Silo recreate it; otherwise run the binary that
  wrote version 3, or migrate the database by hand
  ```

This only catches a version *this build* wrote and a later or earlier build
disagreeing about — it is not a migration system, for either case above.
`SchemaVersion` bumps only for a change to `siloSchema` that `IF NOT EXISTS`
cannot apply safely to an existing database: a rename, a drop, a type change.
A purely additive change (new table, new index) needs no bump.

**Version 2** is the current stamp. It bumped from 1 when `client_kdf_params`
was added to `AccountPassword` — a column on a table that may already exist,
which is exactly what `CREATE TABLE IF NOT EXISTS` cannot apply. The three
tables that landed with it (`AccountIdentityKey`, `AccountRecoveryWrap`,
`ServerSecret`) are additive and would not have needed one on their own. A
database stamped `1` is refused at startup with the message below; since Silo
has no deployments, the answer is to delete it and let the server recreate it.

You can inspect or clear the stamp directly:

```sh
sqlite3 <data-dir>/silo.db 'PRAGMA user_version'          # what it's stamped as
sqlite3 <data-dir>/silo.db 'PRAGMA user_version = 0'       # forget the stamp
```

Clearing it does not make an incompatible database compatible — it only
removes the fast, clear refusal in favour of whatever error the schema
statements produce on their own, which is where every version predating this
check already stood.

## Upgrading from a two-database install

Every release up to and including 0.4.4 kept two SQLite files, `ccnet.db`
(users, groups) and `seafile.db` (libraries, shares, tokens). The merge into one
`silo.db` landed in 0.5.0. Note that 0.4.5 and 0.4.6 were bumped in source but
never tagged, so a build can report either and be on either side of the merge —
go by what is in the data directory rather than by the version string.

A server started on a data directory that still has the old pair refuses to
start rather than creating an empty `silo.db` beside them, and prints the
commands to fold them together. No table name is shared between the two, so
they concatenate:

```sh
sqlite3 <data-dir>/ccnet.db   'PRAGMA wal_checkpoint(TRUNCATE)'
sqlite3 <data-dir>/seafile.db 'PRAGMA wal_checkpoint(TRUNCATE)'
{ sqlite3 <data-dir>/ccnet.db .dump
  sqlite3 <data-dir>/seafile.db .dump; } | sqlite3 <data-dir>/silo.db
```

The checkpoints are not optional: both files run in WAL mode, so a dump taken
without one silently omits every transaction since the last checkpoint.

The next start adds any table or column the schema has gained since. Keep the
old files until you have confirmed the server comes up with your users and
libraries intact.
