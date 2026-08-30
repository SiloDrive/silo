# A missing object reports "Library not found", and tells clients to delete data

**Fixed** in 0.4.3. `libmgr.GetWithReason` now says which of the four
conditions it hit, `libmgr.StatusFor` maps them, and every endpoint that
answers a client uses both. Faults are logged once per library per five
minutes. See [What was done](#what-was-done) at the end for the parts that were
left alone and why.

**Found:** 18 Aug 2026, against 0.4.2, by accident — a `git filter-branch` in the
porter-fuse working tree deleted a tracked copy of the server's data directory
out from under a running `silo serve`. Unintentional fault injection, and a more
honest one than a test would have been: the database survived intact and the
object store did not, which is exactly the shape a half-restored backup, an
interrupted `rsync`, a bad disk, or an over-eager GC leaves behind.

**Severity:** the failure mode is server-side data loss that instructs clients to
complete it locally. Everything else here is cosmetic by comparison.

## Symptom

A library whose head commit object is missing is reported as **404 "Library not
found"** by every endpoint that resolves it, while `GET /api/silo/v1/libraries`
continues to list it, with a `head_commit_id` naming the object that is not
there.

```
GET /api/silo/v1/libraries
  200  [{"id":"328be500-…","name":"Porter Test",
         "head_commit_id":"ac9b78b5c6f7b1905277627bafef0fe99beb15ff", …}]

GET /api/silo/v1/libraries/328be500-…/entries/
  404  Library not found

[fileserver] [ERROR] failed to load commit 328be500-…/ac9b78b5… :
  open share01/storage/commits/328be500-…/ac/9b78b5c6… : no such file or directory
```

The two surfaces disagree because they read different things. `ListLibrariesHandler`
answers from `LibraryOwner`/`LibraryInfo`/`Branch` and never touches the object store,
so a library with an intact row lists cleanly no matter what state its storage is
in. Everything else goes through `libmgr.Get`, which reads the head commit.

## Cause

`libmgr.Get` (`fileserver/libmgr/libmgr.go`) returns `nil` for four
unrelated reasons:

| condition | line | what it actually means |
|---|---|---|
| SQL prepare/query/scan failed | 85, 92, 105 | the database is unavailable |
| no row | 109 | the library really does not exist |
| `HeadCommitID == ""` | 113 | the row is corrupt |
| loading the head commit failed | 136 | the object store is missing an object |

Every caller then does the same thing with that `nil`:

```go
library := libmgr.Get(libraryID)
if library == nil {
    http.Error(w, "Library not found", http.StatusNotFound)
    return nil
}
```

— `entryLibrary` in `fileserver/entries.go`, and the same shape in
`fileserver/api/api.go`, `fileserver/api_handlers.go` and
`fileserver/api/changes.go`, and the compatibility lane's download-info
handler (since deleted with that lane).

Three of those four conditions are the server's own failures. All four are
reported to the client as the one condition that is a statement about *the
client's request*.

## Why this is worse than a wrong status code

404 is not a neutral "something went wrong". To a sync client it is a positive
assertion with an obvious action attached: **this library is gone from the
server, so remove it locally.** That is the correct handling of a real 404 — it
is how a library deleted from the web UI propagates — and it is catastrophic
handling of a missing object. The server loses an object; the client is told the
library was deleted; the client deletes its copy; the copy it deletes is the one
that could have restored the object.

The clients in flight both walk into it:

- **porter-fuse** maps 404 to `ENOENT`. The library would disappear from the
  mount while `/libraries` kept listing it — a directory the mount says is not there
  and the API says is. Under 5xx it maps to `EIO`, which is the truth: something
  is broken, nothing has been deleted, do not act on it.
- A **File Provider** extension has the sharper version of the same problem. A
  404 during enumeration is how deletions arrive, and `NSFileProviderItem`
  removal is what it triggers.

The `docs/protocol.md` § Writing a client guidance to require 0.4.1 and fail loudly rests on the
idea that a confusing failure is worse than a stopped one. This is that argument
applied to a status code: a client that stops cannot make it worse, and a client
that believes a 404 can.

## Suggested fix

Separate the conditions. `Get` already knows which one it hit — the information
is thrown away at the `return nil`.

1. Have `libmgr.Get` return `(*Library, error)` with distinguishable errors, or add
   a `GetWithReason`. Absent row is one thing; a commit that will not load is
   another.
2. Map them: no row → **404**. Empty `HeadCommitID`, a head commit that will not
   load, or a database error → **500**, with a body that says the library exists and its
   storage is damaged. `503` is defensible for the database case, since that one
   is expected to recover on its own.
3. Say it once. The error currently logs per request with no suppression —
   twelve identical lines in thirty-four seconds while a client retried. A
   missing object does not become newsworthy through repetition, and the volume
   is what buries the first occurrence.

An integrity check would be a natural companion — walk `Branch` and confirm each
head commit loads — but it is a separate piece of work and this bug does not need
it to be fixed.

## Note on the trigger

The deletion that caused this was mine, not a server fault, and the library was
disposable test data. Nothing here depends on how the objects went missing: the
handling would be identical after a partial restore, and worse, because the
library would not be disposable.

## What was done

**The reason survives the lookup.** `libmgr.GetWithReason` returns
`(*Library, error)` wrapping one of three sentinels — `ErrLibraryNotFound`,
`ErrLibraryCorrupted`, `ErrLibraryUnavailable`. The scan-failure and no-row cases were
also separated: `rows.Next()` returning false with `rows.Err()` set is the
database failing, not an answer about whether the library exists, and the old
code read both as "no such library".

`Get` keeps its `*Library`-or-nil signature and now wraps `GetWithReason`, so the
twenty-odd internal callers that only need the library are untouched.

**The mapping lives in one place.** `libmgr.StatusFor(err)` returns the status
and body: no row → **404**, database → **503**, everything else → **500** with a
body that says the library exists and its storage is damaged. It sits next to
the sentinels rather than in either HTTP package, because both `silod` and
`fileserver/api` serve libraries and two copies of this decision would drift.

Converted: `entryLibrary` (`entries.go` — the Silo v1 surface porter-fuse and the
File Provider extension read), `loadLibraryAndCommit` and the download handler
(`api_handlers.go`), `ListDirHandler` (`api/api.go`), `ChangesHandler`
(`api/changes.go`), and the compatibility lane's download-info handler, which
was deleted outright in `5d4baa0` along with the rest of that lane.

**It is said once.** A fault is logged on first sight and then suppressed for
five minutes per (library, condition); a successful load clears the entry, so a
recurrence after a repair is reported rather than swallowed. This matters
beyond the log: errors also go to Sentry, so twelve lines was twelve reports of
one server fault. `GetEx` shares the suppression, since the sync path reaches
the same missing object on every request.

### Left alone, deliberately

**`ListLibrariesHandler` still lists a library whose head commit is missing.**
Detecting it means loading every head commit on every listing, which is the cost
the handler exists to avoid. The disagreement between the two surfaces is now
harmless — one lists the library, the other says its storage is damaged — where
before it was one listing it and the other saying it was deleted.

**No integrity check.** Walking `Branch` and confirming each head loads is still
the natural companion, and still separate work.

### Tests

`fileserver/libmgr/get_test.go` covers all four conditions against a SQLite
database and a real object store, including removing the commit store out from
under a live library — the accident, reproduced. `fileserver/library_missing_object_test.go`
asserts the handler behaviour that matters: `entryLibrary` and `loadLibraryAndCommit`
answer 500 for a damaged library and still answer 404 for an absent one.

The package's `TestMain` had to go first. It called `os.Exit(0)` when
`TEST_LIBRARY_ID` was unset, which skipped the whole package — anything added
beside it would silently never have run.
