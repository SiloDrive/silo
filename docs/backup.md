# Backup and restore

A Silo deployment is two kinds of state, and they need different handling:

| What | Where | How to copy |
|---|---|---|
| Databases | `<data-dir>/ccnet.db`, `<data-dir>/seafile.db` | `silo backup-db` |
| Object store | `<data-dir>/storage/` | `rsync`, `cp -a`, snapshot, tar — anything |

The object store is an immutable content-addressed file tree; any ordinary
copy tool handles it. The databases are not safe to copy with `cp`, and the
order the two halves are captured in decides whether the backup restores.

## Why `cp seafile.db` is not a backup

The databases run in WAL mode. A committed transaction is durable once it
reaches `seafile.db-wal`; it only moves into `seafile.db` at a checkpoint, and
Silo checkpoints on clean shutdown. Copying only `seafile.db` off a running
server therefore silently drops every write since the last checkpoint — no
error, no warning, and nothing in the restored copy to show that anything is
missing.

Copying `seafile.db`, `seafile.db-wal` and `seafile.db-shm` together is not a
fix either. The three files are read at three different instants, and the set
you end up with need not be a state the database was ever in.

`silo backup-db` uses SQLite's `VACUUM INTO`, which takes a read transaction
over the source and writes one consistent, already-checkpointed file. It is
safe while the server is running and blocks no writer.

## The order rule

**Databases first. Object store second.**

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

This is the mistake a plain `rsync -a <data-dir>/ backup/` makes: rsync walks
alphabetically, so `storage/` is copied before `seafile.db`.

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

The databases are checked cheaply:

```sh
sqlite3 /backup/silo/2026-08-16/seafile.db 'PRAGMA integrity_check;'
```

A snapshot from `backup-db` stands alone: there should be no `-wal` or `-shm`
file beside it. If there is, it did not come from `backup-db`.

To confirm the store matches the heads, restore into a scratch data directory
and run `silo serve` against it — a client that syncs a library end to end has
verified every object that library's head references.

## MySQL and PostgreSQL

`silo backup-db` only handles SQLite and exits with an error on other engines.
Use `mysqldump` or `pg_dump` for the databases; the order rule and everything
said about `storage/` above is unchanged.
