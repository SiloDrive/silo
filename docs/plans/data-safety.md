# Data safety

Status: **items 1–5 and 10 built.** A review of the store, the reclaimers, the ingest
path and the catalog on 2026-09-02, written as a work order. Each item below is
one bug or gap, its evidence, the test that has to fail before the fix, and the
fix. Tick the status line as each lands; an item that turns out to be wrong
gets a line saying why rather than being deleted.

The rule for every item is the one in `CLAUDE.md`: write the test, watch it
fail, then fix, then run the suite. Where the failing test cannot come first
the item says so.

**What the review found sound, so nobody re-checks it:** every write is hashed
against its id before it is published (`objstore.go` `WriteVerified`,
`WriteBatch`); frames are AEAD-bound to id and path; objects are fsynced before
the branch head moves; the head move is a compare-and-swap with the GCID check
inside one transaction on a single write connection; server-side mutations are
objects-then-head everywhere; the catalog holds no object location; compaction
publishes by rename with the new pack and its directory durable before the old
one is unlinked; `storage.key` is generated from `crypto/rand`, linked rather
than overwritten, and never logged. The storage packages pass under `-race`.

Findings marked **confirmed** were read in the source during the review.
**Reported** means the reviewer's reading was not independently re-checked;
the test is what confirms or clears it.

---

## Tier 1 — acknowledged data can be lost or hidden

### 1. The generation guard did not guard the case it was for

Status: **fixed.** Confirmed, and wider than first read.

The review found `sweepOrphans` and `compactLibrary` reading the head before
bumping the generation, so a commit landing between the two passed the check
while the mark walked the old head. The fix for that alone would have been a
swap. Writing the test showed the swap closes nothing: a client that reads the
generation *after* the bump also passes, and on the HTTP path the generation
is read inside `putHeadHandler`, milliseconds before `updateBranch`, so it was
never protecting anything the client did before that moment. The age guard
does not help either, because the chunk a resumed upload finds by
`check-blocks` may have sat unreferenced for a month. A stalled-then-resumed
upload racing a collection lost its chunks and kept its head.

**Test.** `fileserver/gc_window_test.go`, three tests, all failing before the
change. A commit started during a sweep and a commit started during a
compaction, each asserting that either the commit is refused or the
collection saw it; the compaction one showed the chunk gone after a landed
commit. And a generation read mid-collection must be stale once it ends.

**What was done.** A collection is now a window: `beginCollection` stamps a
marked generation before the head is read, `updateBranch` refuses every head
move while the marker is set, and a deferred `endCollection` mints a fresh
unmarked one after the last removal. Dry runs of expiry and compaction mark
nothing; a reporting sweep still does. `putHeadHandler` reads the generation
before it checks anything exists. A collection that dies leaves the marker,
and head moves stay refused until the next `silo gc`. Docs in `storage.md`
§ Reclaiming say the same.

### 2. A failed seal orphans every acknowledged frame in the open pack

Status: **fixed.** Confirmed.

`rotate` called `clearOpen()` before `p.seal()`. If the footer write or its
fsync failed — ENOSPC is the realistic one — the pack was in neither the open
slot nor the sealed list: its bytes durable, every lookup missing, the next
write opening a second pack, and the next restart refusing the library for
having two. Even on success every seal had a window the length of an fsync
during which the whole pack was absent.

**Tests.** `TestNoLookupMissesDuringASeal` and
`TestASealFailureKeepsAcknowledgedObjectsReadable` in
`fileserver/objstore/packwrite_test.go`, both failing before the change, both
through a seam inside `seal` (`packSealHook`) that lands a lookup or a failure
between the decision and the footer write.

**What was done.** `seal` marks the pack sealed under its lock, snapshots the
index, and does its I/O outside the lock without closing the file. `rotate`
seals first, then swaps the sealed pack in and the open pack out under one
lock hold, then closes the file. A reader that took the open pack before the
swap and reads after the close gets `os.ErrClosed`, and the store's read path
asks the set again on exactly that error. On any failure the pack stays where
it was, marked sealed so `append` refuses and the next writer retries the
seal; the sidecar's close is idempotent so the retry works. The compaction
rewrite closes the file it now owns.

### 3. A truncated upload body is answered 200

Status: **fixed.** Confirmed.

`readObjectBody` returned `false` on a body read error without writing a
status, on the reasoning that a client that hung up has not asked for an
answer. A half-close, a proxy cutting the upstream, or an HTTP/2 stream ending
early read the same way, and in all of those the client is listening.
`net/http` then sent 200 for an object that was never stored.

**Test.** `TestATruncatedObjectPUTIsNotAnswered200` in
`fileserver/truncated_body_wire_test.go`, over a raw socket because no client
library sends a short body on purpose. Both `objects/{id}` and `chunks/{id}`
answered 200 before the change.

**What was done.** A body that ends early is a 400. `putEntryFile` on the
plain-library path reaches a 500 through `WriteFile` rather than a 200, so it
was left alone.

### 4. A head move checks the commit and its root and nothing below

Status: **fixed.** Confirmed.

`putHeadHandler` decoded the commit and stat'd its root. `MeasureDelta` read
every changed directory and manifest on the way to the totals, so a missing
one of those failed the move with a 500 by accident; chunks were never
checked. A manifest naming a chunk nobody uploaded published fine and the hole
surfaced on another device at read time.

**Tests.** `TestAHeadMoveOverAMissingChunkIsRefused` (200 before the change)
and `TestAHeadMoveOverAMissingSubdirectoryIsRefused` (500 before) in
`fileserver/head_verify_test.go`.

**What was done.** `objmgr.Store.VerifyDelta` walks the same delta the
accounting does — added and changed entries only — and checks every
directory, manifest and chunk beneath them, returning a typed
`MissingObject`. The head move runs it after reading the generation and
before measuring, and answers 400 naming the kind and id. It needs no key.

### 5. Append writes at the file position, not the indexed offset

Status: **fixed.** Confirmed.

`openPack.append` wrote with `Write` and indexed at `p.size`, which only
advanced on full success. After a short write the kernel position and the
index disagreed, and every later frame was acknowledged at an offset that did
not hold it.

**Test.** No failing test first: producing the short write needs the file
seam in item 12, which is not built. The existing recovery test
(`TestRecoveryTruncatesToTheLastIndexedFrame`) simulated a torn write by file
position and was changed to write at the indexed offset, which is what a
crashing append now does. Item 12's test still belongs here when the seam
exists.

**What was done.** `WriteAt(frame, p.size)`, and `append` refuses once the
pack is marked sealed, so nothing lands past a footer.

### 6. Two servers on one data directory destroy each other's open pack

Status: **fixed.** Confirmed.

Nothing locked the data directory (`server.go` took no lock; `-P` writes a
pidfile nobody reads). A second instance reached `loadPackSet`, saw the
first's sidecar, and `recoverPack` truncated the live pack to its last indexed
record while the first was mid-append. The second then sealed it, writing a
footer at its offset and unlinking the sidecar the first still held open.
The first kept appending past the footer. Next start: no sidecar means
sealed, the trailer check fails, and the library is `ErrPackCorrupt`
including every acknowledged frame.

**Tests.** `TestSecondProcessCannotAdoptALiveOpenPack` in
`fileserver/objstore/datalock_test.go` is the one this item named, and it
decides the behaviour as much as it checks it: a first holder with a live open
pack, a second refused, and the first's acknowledged frame still found
afterwards. Beside it, `TestTheDataDirRefusalNamesTheHolder`,
`TestReleasingTheDataDirLetsTheNextProcessIn`, and
`TestALeftoverLockFileIsNotALock`. All four were watched failing against a
`LockDataDir` stubbed to take no lock, which is the behaviour they replaced —
a compile error would have proved only that the symbol was missing.

**What was done.** `objstore.LockDataDir` takes an exclusive non-blocking
`flock` on `{data-dir}/silo.lock` and returns a `DirLock`; `Release` drops it
and leaves the file, because unlinking would free an inode another process is
already waiting on and let two holders in. `Run` takes it immediately after
`resolvePaths` — before the pidfile, so a refused start does not overwrite the
running server's, and before anything opens a pack — and releases it on the
deferred path, which runs after the packs are sealed and the database is
closed.

`flock` rather than a pidfile: the lock belongs to the open file description,
so the kernel drops it however the process ends, and a leftover file is not a
lock. The pid written inside is read only to name a holder in the refusal,
never to decide whether one exists — verified by killing a server with
`SIGKILL` and starting another over the file it left.

`gc -delete` and `gc -compact -delete` take the same lock, which turns two
warnings into a check and retires `compactionIsOffline`. A reporting pass
takes nothing: it changes nothing, and what a running server would reclaim is
a fair question to ask.

## Tier 2 — the store can be left refusing, or reclaim the wrong thing

### 7. Compaction copies frames blind and prune trusts the index

Status: **open.** Reported.

`sealedPack.readFrameAt` does a raw `ReadAt` with no id or AEAD check, and
`copyFrames` in `fileserver/objstore/packcompact.go` appends whatever came
back. `pruneRedundant` (`:309`) deletes a pack because another pack's index
lists its ids, without opening a single one. A torn frame in the survivor
means the only good copy is deleted, or the corruption is carried forward and
the original removed.

**Test first.** `TestCompactionRefusesToCopyAFrameThatDoesNotOpen`: corrupt
one live frame's bytes in a sealed pack; compact; expect an error, old pack
untouched, other frames readable. And
`TestPruneKeepsThePackWhoseCopyActuallyOpens`: two packs sharing an id, the
copy in the larger corrupted; prune must not delete the smaller.

**Fix.** `openFrame` each frame during copy — the header check is enough to
catch a torn frame, and the AEAD needs no key decision because the store key
is the one that sealed it. Prune opens one frame per shared id, or every
frame if the pack is small.

### 8. Retention trusts the client's `created_at`

Status: **open.** Reported.

`commitsBehindTheWindow` in `fileserver/gc_expire.go` compares the commit's
`CreatedAt`, which a client sets for uploaded commits (`putHeadHandler`), to a
server cutoff, and the prefix rule then dooms every ancestor. A client whose
clock is a month behind expires its own five-minute-old commit and the whole
chain behind it on the next run; on a packed store `-compact -expire-history`
carries that out irreversibly.

`docs/storage.md` § What the catalog holds already says the catalog's time is
the one that is not a claim. `RecordHeadMove` in `commit.go` writes it.

**Test first.** `TestAClientClockInThePastCannotExpireFreshHistory`: upload a
commit with `created_at` a year old atop a real chain; `expireHistory(14d,
true)`; assert only commits whose server-observed head-move time is behind the
cutoff are expired.

**Fix.** Cut on the server-observed time. The head-move log has one row per
head; commits that were never a head (merge parents uploaded together) inherit
their child's time, which errs toward keeping.

### 9. The mark for a virtual library is that library's head alone

Status: **open.** Reported, latent.

`liveLibraryIDs` returns every `Library` row. A virtual library's `StoreID`
is its origin, so the sweep, expiry and compaction open the origin's store and
walk only the virtual head. Whatever the origin's head reaches that the
sub-view does not is unreferenced from the virtual side, and the origin's pass
sees the virtual library's own commits as garbage. `gc.go` reasons about
shared stores for dead-library reclaim; the three live-library reclaimers do
not. Only tests create `VirtualLibrary` rows today, but the loader and the
share code honour them, so this is a bug waiting on a feature.

**Test first.** `TestSweepingAVirtualLibraryNeverTouchesTheOriginsHead`:
origin with files, a `VirtualLibrary` row on a subdirectory with its own head;
`sweepOrphans(virtualID, 0, true)`; assert the origin's census is unchanged
and every origin file reads.

**Fix.** Run each reclaimer once per store, with the mark rooted at the union
of every head whose `StoreID` is that store.

### 10. A pack recovered at startup is never sealed by age

Status: **fixed.** Confirmed, and one more cause than first read.

`recoverPack` left `first` zero, which `olderThan` reads as never old. And the
age sealer only started on the first write, so a process that only read never
ran it at all: a read-only library after a crash kept its last frames outside
a sealed pack for ever, whichever of the two was fixed alone.

**Test.** `TestARecoveredPackIsSealedByTheSweeper` in
`fileserver/objstore/packwrite_test.go`: write, discard the store the way a
kill would, reopen, read once, wait past the age. Failing before the change.

**What was done.** Recovery stamps `first` with now — the latest honest value
— and loading a library whose open pack holds frames starts the sealer.

### 11. An unsynced batch can leave the index ahead of the data, and recovery refuses

Status: **open.** Reported.

`appendBatch` appends each frame with `sync=false` and fsyncs once at the end.
Two files, no ordering: a power cut mid-batch can persist a sidecar record
before its frame bytes. `recoverPack` in `pack.go` treats "pack shorter
than index" as impossible and returns `ErrPackCorrupt` for the whole library.
Nothing acknowledged is lost — the batch was never acknowledged — but the
remedy is manual sidecar surgery, and the comment's premise is false for this
path. Same outcome for any run with `SILO_SYNC_OBJECT_WRITES=false`.

A second half: on a mid-batch error the earlier frames are already in `byID`,
so a retry's `HasChunk` says present and the client commits over bytes that
may not be on disk.

**Test first.** `TestIndexAheadOfDataAfterUnsyncedBatch`: append a batch with
`sync=false`, truncate the pack under the last record with the full sidecar;
`loadPackSet` should truncate the sidecar to the data and continue. And
`TestAFailedBatchLeavesNothingFindable`: inject a failure on the Nth frame;
assert `Exists` is false for every frame of the batch.

**Fix.** In recovery, trim the sidecar to the last record the pack fully
holds, the same way a torn trailing record is dropped. In `appendBatch`, on
error remove the batch's ids from `byID` and truncate the sidecar back, or
fsync before indexing.

### 12. The open pack needs a file seam for crash injection

Status: **open.** Not a bug; the tool items 2, 5 and 11 need.

`openPack.f` and `sidecar.f` are `*os.File`. Items 2, 5 and 11 cannot write
their failing test without a way to make one write short or one fsync fail.
`packs.md` § "The layout is implemented twice" already proposes exactly this
interface.

**Do.** A small interface — `WriteAt`, `Sync`, `Truncate`, `Close`, `ReadAt`
— satisfied by `*os.File`, with a test-only wrapper that fails on the Nth
call. No behaviour change; the existing suite is the test.

## Tier 3 — catalog, keys, backup, docs

### 13. Replacing an identity key strands every E2EE library it holds

Status: **open.** Reported.

The only wrap of an E2EE content key is the owner's, and `account.SetKeys`
in `fileserver/account/keys.go` `INSERT OR REPLACE`s the identity row
including `public_key` with no check that the key is unchanged while
`LibraryKeyWrap` rows exist for that account. A client that mints a fresh
keypair instead of re-wrapping the old one gets 200 and has lost every
library it owns.

**Test first.** `TestPublishingADifferentIdentityKeyIsRefusedWhileWrapsExist`:
create an E2EE library, PUT `account/keys` with a new `public_key`; assert 409
and that the wrap still opens with the original key.

**Fix.** Refuse a `public_key` change while wraps exist unless the request
carries re-wraps for every one of them, applied in the same transaction.

### 14. `backup-db -f` removes the old backup before the new one exists

Status: **open.** Confirmed.

`RunBackupDB` in `fileserver/backup.go` removes `dst`, then `VACUUM INTO`. A failure
mid-vacuum leaves no backup, which the comment above it says is worse than
none. No `integrity_check` runs on the output.

**Test first.** `TestBackupDBForceKeepsOldCopyUntilNewOneExists`: point `src`
at something that makes the vacuum fail; with `-f`, the old file survives. On
success, the output passes `PRAGMA integrity_check` and holds the last
committed row.

**Fix.** Vacuum into `dst.tmp` in the same directory, run `integrity_check`
on it, rename over `dst`.

### 15. `DeleteLibrary` swallows the `GarbageLibraries` insert error

Status: **open.** Reported.

`DeleteLibrary` in `libmgr.go` logs and continues. The rows are gone, the directory stays,
and nothing will ever collect it.

**Test first.** `TestDeleteLibraryFailsWhenTheGarbageRowCannotBeWritten`:
make the insert fail; assert `DeleteLibrary` returns an error and the
`Library` row remains.

**Fix.** Return the error; the transaction rolls back.

### 16. A library id can be reclaimed by GC after it was re-created

Status: **open.** Reported, narrow.

`collectGarbageLibraries` decides on the `Library` row and `reclaim` deletes
the directory later, with no transaction across the two. E2EE ids are
client-chosen and reusable after delete. A create with the dead id in the gap
has its root and commit removed. Related: a request that loaded the `Library`
before the delete and writes after the reclaim recreates the store directory
with no garbage row, and it is never collected.

**Test first.** `TestGCDoesNotReclaimAnIDRecreatedAfterItDecided` with a hook
between decide and reclaim; and
`TestAnUploadInFlightAcrossDeleteDoesNotResurrectTheStore`.

**Fix.** Re-check the `Library` row inside `reclaim`, per id, immediately
before the remove. For the resurrection, the write path checks the library
row exists before creating a store directory, or the data-dir lock in item 6
plus a startup sweep of directories without rows.

### 17. The mark must abort on an unreadable pack, never skip

Status: **open.** Reported sound; wants a test.

The census returns a plain error on a pack I/O error and treats only
`ErrNotFound` as the end of a branch. That is the safe direction, but nothing
pins it, and a future "skip unreadable" would make the sweep delete the
children of whatever it could not open.

**Test.** `TestMarkAbortsOnAnUnreadablePackRatherThanSweeping`: chmod 000 or
truncate a sealed object pack holding a directory; `sweepOrphans(del)` returns
an error and removes nothing — no `RemoveOrphan` calls, packs byte-identical.

### 18. Compaction crash matrix

Status: **open.** Reported sound; wants tests.

The order is right: tmp pack sealed and fsynced, renamed, directory fsynced,
set swapped under one lock, old pack removed, directory fsynced. A crash after
the rename leaves both packs; prune keeps the larger, which is the old one
holding the dead ids, so the rewrite is rolled back and redone — no loss, and
a deterministic crash there makes no progress. Only
`TestAnInterruptedRewriteLeavesTheOldPackAuthoritative` covers one point.

**Tests.** `TestCompactionCrashAfterRenameRestartsToAReadableStore` and
`TestCompactionCrashAfterOldPackRemovedBeforeDirSync`, both through a fault
point in `CompactPack`; reload; every live frame reads; one pack per id after
prune; a second run reaches the intended end state.

### 19. One corrupt sealed pack takes the whole library offline

Status: **open.** Confirmed.

`loadPackSet` in `packstore.go` fails the library if any sealed pack does
not open, retried on every call. Every object in the library — other packs,
loose files — becomes unreadable and unwritable because one pack has a bad
tail. Related: a header-only `.pack` without a sidecar, which a crash between
`createPackNamed`'s two steps or `discard`'s two removes can leave, is read as
sealed and rejected the same way.

**Tests.** `TestOneCorruptSealedPackDoesNotTakeTheLibraryOffline`: damage one
sealed pack's tail among several; other packed and loose objects read; the
damaged pack's objects report corruption, not `ErrNotFound`.
`TestAHeaderOnlyPackWithoutASidecarDoesNotPoisonTheLibrary`.
`TestASidecarWithoutAPackIsCorruptionNotAbsence`: today a missing `.pack` with
its `.idx` present is silently ignored and its objects become not-found, which
tells the client they were never stored.

**Fix.** Load what opens, hold the failed pack's ids as "present, corrupt",
and log once. Reverse `discard`'s removal order (an orphan `.idx` is already
ignored). Tolerate a header-only pack at load.

### 20. Schema creation is not one transaction

Status: **fixed.** `loadSchema` in `fileserver/dbutil/migrate.go` creates the
record table, the schema and every migration row in one transaction, and
`TestAFailedSchemaLoadLeavesNothingBehind` pins that a failed load leaves no
tables for the next start to refuse.

`CreateSiloTables` runs each statement with `db.Exec` and stamps
`user_version` afterwards. A crash between leaves tables present and version
zero, which the next start reads as "written before this build's schema" and
refuses with no migration path. The message blames the wrong thing.

**Test first.** `TestSchemaCreationIsAtomic`: inject a failure mid-schema;
reopen; expect either the fresh path or a stamped DB, never the refusal.

**Fix.** One transaction around creation and the stamp.

### 21. No fuzz tests

Status: **open.**

`spec/store-format.md` pins parse bounds for manifests, directories, commits,
chunk frames and the pack footer and sidecar, and there is not one `Fuzz`
function in the tree. Targets, in order: `DecodeManifestPublic`,
`DecodeDirectoryPublic`, `DecodeCommitPublic`, `ReadChunkFrame`,
`openFrame`, the footer parser, the sidecar reader, `ParseID`. Seed each from
the existing vectors. A decoder that panics on a short read is a crash a
client can trigger with one upload; one that reads past a bound is a mark that
walks garbage.

### 22. Documentation that no longer holds

Status: **open.**

- `docs/backup.md` says the store is an immutable file tree any copy tool
  handles. An open `.pack` and its `.idx` are not, and the safe copy order
  between them is the reverse of rsync's alphabetical one only by luck of
  the names. Say: an open pack copied mid-append restores as whatever prefix
  recovery keeps, which is fine because those frames postdate the DB
  snapshot; and item 11's fix makes the other order safe too.
- The "no `gc -delete` during a backup" rule must also cover
  `-compact -delete`, which renames a new pack in and unlinks the old, so a
  concurrent copy can hold neither. `--link-dest` still deduplicates sealed
  packs.
- `backup.md` lists the database as holding branch heads. It also holds the
  only server-side copy of every E2EE content key wrap, every account identity
  wrap and every recovery wrap. Losing it loses every E2EE library whose
  members have no local keyring. Say so, beside the `storage.key` warning.
- `docs/storage.md` § Where the bytes are says pre-frame stores are discarded,
  not migrated, but nothing detects one: with the key present the server
  starts and fails per read; with it missing the error blames the key. A
  store version marker, or a refusal when the first loose object is not a
  frame, would make the doc true.
- `df` and `gc` open the store through `openStores`, which generates a
  `storage.key` when the tree is empty. Run against a mistyped `-d` they mint
  a key into the wrong directory. Either say so, or have the read-only
  commands refuse to generate.

---

## Order

1. ~~Items 1, 2, 3, 4~~ — done, each behind a test that failed first. Item 2
   got its seam inside `seal` rather than waiting for item 12.
2. Items 6, 7, 8 — the ones that turn a bad day into a refused library or a
   wrong deletion.
3. Item 12, the file seam, then the item 5 and item 11 tests that need it.
4. Items 9, 11, 13, 14, 15, 16, 19, 20 — in any order.
5. Items 17, 18, 21 — tests that pin what is already right.
6. Item 22 — docs, last, so they describe what was built.

Items that grow past a paragraph of *What was done* move to `docs/bugs/`
under the convention in its README, with this file linking to them.
