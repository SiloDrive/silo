# Backup and restore

A Silo deployment is three kinds of state, and they need different handling:

| What | Where | How to copy |
|---|---|---|
| Storage key | `<data-dir>/storage.key` | copy it once, somewhere else, now |
| Database | `<data-dir>/silo.db` | `silo backup-db` |
| Object store | `<data-dir>/storage/` | `rsync`, `cp -a`, snapshot, tar — anything |

The object store is an immutable content-addressed file tree; any ordinary
copy tool handles it. The database is not safe to copy with `cp`, and the
order the two halves are captured in decides whether the backup restores.

## `storage.key` first, and only once

**Every object in the store is encrypted under `storage.key`, and it cannot be
rotated.** It is 32 bytes, generated the first time the server starts, and
without it a backup of the object store is a directory of unreadable files —
a complete backup by every measure a backup tool applies, and worth nothing.

It never changes, so it is not part of the nightly rotation: copy it once, to
somewhere that is not this machine and not the same disk as the store, and
check that you still have it whenever you check anything else. A password
manager or a printed copy is a reasonable place for 32 bytes.

The server prints a one-time warning when it generates the key. If the file
goes missing while the store still holds objects, the server **refuses to
start** rather than generating a new one — a fresh key would look exactly like
a clean first start and the loss would not surface until the first read of an
old object.

There are two ways out and the error names both: restore the key from wherever
it was copied, or delete `<data-dir>/storage/` and let clients re-upload. The
second is a real answer only while this server has no deployments to speak of,
and it stops being one the moment it does — which is the whole reason the first
one is worth the two minutes it takes.

The other side of that rule: a copy of `storage.key` is a copy of everything
needed to read the store, so treat it the way its contents deserve. It is
mode `0600` in the data directory, and it belongs somewhere at least that
careful.

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

`storage.key` is not in there, deliberately — see above. It is copied once, by
hand, somewhere else.

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
cp /wherever/you/kept/it/storage.key /var/lib/silo/storage.key
chmod 600 /var/lib/silo/storage.key
systemctl start silo
```

**The key goes back too, and it is the same key.** A restore that puts back
the database and the store but not `storage.key` restores nothing readable,
and the server will say so rather than starting: a missing key over a store
that holds objects is a refusal, not a first start.

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

Check the key is where you think it is, and that it is 32 bytes:

```sh
wc -c < /wherever/you/kept/it/storage.key    # 32
cmp /wherever/you/kept/it/storage.key /var/lib/silo/storage.key
```

This is the one check with no second chance, and the cheapest one here.

To confirm the store matches the heads, restore into a scratch data directory
and run `silo serve` against it — a client that syncs a library end to end has
verified every object that library's head references.

## Schema migrations

`silo.db` records which migrations produced its shape, in a `SchemaMigration`
table with one row per applied migration name. `fileserver/dbutil/schema.go`
holds the schema a fresh database is created in; `migrate.go` beside it holds
the ordered list of migrations that take an older database to the same place.

What happens at startup depends on what is already in the file:

- **No tables** — a fresh database. The schema is loaded and every migration
  is recorded as applied, in one transaction, so a crash partway leaves
  nothing for the next start to trip over.
- **Tables, but no `SchemaMigration`** — written before migrations were
  tracked. Refused: nothing can say what shape it is in. Silo had no
  deployments when the record landed, so the answer is to delete it and let
  the server recreate it.
- **A record naming a migration this build does not list** — a newer build
  has migrated it. Refused before any statement runs, naming the migration;
  run the binary that wrote it.
- **Migrations this build lists that the record lacks** — the database is
  behind. `silo serve` applies them in order on start, each in its own
  transaction with its row written last, so a migration either happened and
  says so or did not happen. Every other command refuses a database that is
  behind rather than migrating it beside a server that may be running.

`silo migrate` runs the same code without starting the server, for an
operator who wants to watch it succeed before opening the port. It takes the
data directory lock, so it refuses while a server is up. `silo migrate -n`
lists what would run and runs nothing.

```sh
silo backup-db /backup/silo/$(date +%F)   # first, always — the backup is the down migration
silo migrate -n                           # what this build would do
silo migrate                              # do it
```

There are no down migrations. The backup taken before the upgrade is the way
back, which is why the order above is the order.

To inspect the record directly:

```sh
sqlite3 <data-dir>/silo.db 'SELECT name, datetime(applied_at, "unixepoch") FROM SchemaMigration'
```

There is no upgrade path from any earlier release — including the two-file
databases of 0.4.4 and before — and none is planned: there are no deployments,
so there is nothing to migrate; delete the data directory and let the server
recreate it (see [`storage.md`](storage.md)).
