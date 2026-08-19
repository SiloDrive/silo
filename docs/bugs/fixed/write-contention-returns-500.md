# Concurrent writes to one directory return 500 after ~14 seconds

**Fixed** in 0.4.4. Contention now has a sentinel (`ErrRetriesExhausted`), one
mapping function (`writeCommitErr`) decides what a client is told, and a lost
race answers **503** with `Retry-After: 1` instead of 500. The backoff was
rebuilt as well, which is where the 14 seconds went. See
[What was done](#what-was-done) at the end, including a second instance of the
bug that was worse and is also fixed, and one thing left alone.

**Found:** 18 Aug 2026, against 0.4.2, while seeding a test library with a wide
directory — 3000 files written into one directory by 16 concurrent writers.

**Reproduces reliably:** 74/3000 (2.5%) at 16 writers, 15/1200 (1.2%) on a second
run. Zero failures at 300 files with either 4 or 16 writers, so it is a function
of how long the contention lasts, not of directory size. Every failed write
succeeded on a serial retry, unchanged.

## Symptom

```
3000 files, 16 concurrent writers:  {201: 2926, 500: 74}
  500 body: "Internal server error"

1200 files, 16 concurrent writers:  {201: 1185, 500: 15}
  201: n=1185  median 0.02s  max 20.67s
  500: n=15    median 14.09s max 21.09s
```

The client waits a **median of 14 seconds** to be told the server is broken. It
is not broken. The write lost a race for the branch head ten times running.

## Cause

`GenNewCommit` (`fileserver/fileop.go:1942`) retries a lost race up to ten
times, sleeping a random 100–3000 ms between attempts:

```go
if retryCnt < maxRetryCnt {
    /* Sleep random time between 0 and 3 seconds. */
    random := rand.Intn(30) + 1
    time.Sleep(time.Duration(random*100) * time.Millisecond)
    ...
} else {
    err := fmt.Errorf("stop updating repo %s after %d retries", repoID, maxRetryCnt)
    return "", err
}
```

That error is an ordinary `error`, so `putEntryFile` cannot tell it from a real
one and takes the default branch (`fileserver/entries.go:443`):

```go
if errors.Is(err, ErrGCConflict) {
    http.Error(w, "GC conflict; retry", http.StatusConflict)   // ← right
    return
}
log.…Errorf("failed to commit %s in repo %s", path, repoID)
http.Error(w, "Internal server error", http.StatusInternalServerError)  // ← wrong for this case
```

The correct handling for the neighbouring contention case is four lines above it.
GC conflict already gets a 409 that says "retry" in the body. Branch-head
contention is the same kind of event and gets the status code that means the
opposite.

Ten retries × up to 3 s explains the 14 s median directly, and the 20 s maximum
on a *successful* write shows the loop working as designed — those are writes
that won on the eighth or ninth attempt.

## Why the status code matters

500 is the one class a client must not retry blind. It means the server hit an
unexpected condition and may have applied part of the request; the safe response
is to stop and surface it. 503 means the opposite — transient, come back — and
carries `Retry-After` to say when.

So every client does the wrong thing:

- Obeying the 500 and giving up **loses a write that would have succeeded**.
- Retrying anyway is guessing, and the guess is wrong the moment a real 500
  appears.
- **porter-fuse** maps 5xx to `EIO`. A `close(2)` returning `EIO` is data loss
  from the application's point of view — it has already written the bytes and
  has nowhere to put them. It would be reporting that on a write the server
  would have accepted a second later.

The 14-second wait compounds it: a client with a request timeout shorter than
that never sees the status at all, only a timeout, and has even less to go on.

## Suggested fix

1. Give the exhaustion case a sentinel — `ErrRetriesExhausted`, beside the
   existing `ErrConflict` and `ErrGCConflict` — so callers can distinguish
   "contention" from "something broke".
2. Map it to **503** with `Retry-After: 1`, or **409** to match the GC-conflict
   precedent in the same function. Either is defensible; the important part is
   that it is not 5xx-unexpected. A body of "write contention; retry" makes it
   actionable.
3. Consider whether the sleep belongs on the request path at all. Ten attempts
   averaging 1.5 s means a contended write can hold a connection for 30 seconds
   before answering. Shorter backoff with the same attempt count would fail
   faster and let the client — which knows its own deadline — decide how long to
   keep trying.

The retry loop itself looks right. `maxPostFilesRetries`'s comment already
reasons carefully about matching the inner limit; this is only about what
happens at the end of it.

## Not a bug, for the record

Two things found in the same session that turned out to be correct behaviour,
noted so they are not re-investigated:

- **A `PUT` into a missing directory is a 404.** Documented in
  `docs/porter-brief.md`: not an implicit `mkdir -p`, because a typo should not
  build a path. Clients must create parents themselves.
- **No upload size limit was hit.** A 50 MB `PUT` returns 201. An earlier
  connection reset at that size was an artifact of a broken object store, not a
  limit.

One genuinely open question, low priority: **a filename containing a newline is
rejected with 404** while every other awkward name tested — accented, CJK,
emoji, `#`, `&`, `+`, `\`, leading `-`, doubled spaces, trailing space, quotes,
200 characters — is accepted. A newline is legal in a POSIX filename, so a FUSE
client can be asked to create one. Worth deciding whether that is a deliberate
rejection or a side effect of request parsing, and if deliberate, whether 400
would describe it better than 404.

## What was done

**Contention is a sentinel, not an ordinary error.** `ErrRetriesExhausted`
joins `ErrConflict` and `ErrGCConflict` in `fileserver/fileop.go`, returned
(wrapped, so the repo id and attempt count survive) from both places a write
gives up: `GenNewCommit` after its ten merges, and `postFilesAndGenCommit`
after its ten re-walks. The second one used to return a bare `ErrConflict`,
which meant the outer exhaustion and an immediate non-replace conflict were
literally the same value.

**One place decides what the client is told.** `writeCommitErr(w, r, err, what)`
sits beside the sentinels — the same reasoning as `repomgr.StatusFor` in
`missing-object-reports-repo-not-found.md`: `entries.go` and `api_handlers.go`
both commit, and two copies of this decision would drift. It maps contention to
**503** + `Retry-After: 1` + `write contention; retry`, GC conflict to its
existing 409, and everything else to 500. It also logs: contention at info,
real faults at error. That last part is not cosmetic — errors go to Sentry, so
the 3000-file seeding run in this report would have filed 74 reports of the
server working as designed.

**503, not the 409 the GC-conflict case uses.** 409 stopped being free three
commits ago: `move-onto-directory-destroys-it.md` gave it to destination
collisions, and Porter maps that to `NSFileProviderError.filenameCollision` —
"return the existing item so the system renames". Answering a contended write
with 409 would tell a File Provider client to rename the user's file. 503 says
transient, `Retry-After` says when, and `macos-fileprovider-plan.md`'s error
table already routes 5xx to `.serverUnreachable` with backoff.

**The same bug, one level worse, also fixed.** Five Silo-lane handlers —
`mkdirHandler`, `deleteFileHandler`, `renameHandler`, `moveHandler` and the
library rename — call `GenNewCommit` with `handleConncurrentUpdate = false`,
which returns `ErrConflict` on the *first* lost race with no retry at all. Every
one of them logged and answered 500. So a concurrent mkdir did not need ten
collisions to produce the reported failure; it needed one. They now go through
`writeCommitErr` too. Their `false` was left as it is: `true` would enable
`mergeTrees` auto-merge semantics for rename and move, which is a contract
change and not this bug.

**The backoff.** The report's third suggestion, taken. The flat "random
100–3000 ms" is now `contentionBackoff(attempt)`: exponential from 50 ms to a
1 s ceiling, with full jitter. The old shape made the first retry wait an
average of 1.55 s to re-take a head the winner had claimed in 20 ms, and put ten
such waits on the request path — which is most of the 14-second median. The new
worst case is bounded by attempts × 1 s, and the common case (contention that
clears in milliseconds) now retries in milliseconds. Full jitter, uniform over
`[0, window)` rather than the window itself, is the part that actually spreads a
herd; the doubling is what stops a persistent loser hammering. `maxRetryCnt`
became `genNewCommitRetries`, a package var, only so a test can set it to zero.

### Left alone, deliberately

**The legacy upload and download paths in `fileop.go` still answer 500.** Same
error, same wrongness, but they are the Seafile lane's and are entangled with
the 444/445 codes the upstream client understands — the same reason
`missing-object-reports-repo-not-found.md` left `fileop.go`'s 400 "Bad repo id"
alone. `fastForwardOrMerge`'s own three-retry exhaustion is in the same
category. Worth a separate pass, with a Seafile client to test against.

**409 is now overloaded, and that is a decision, not an oversight.** A GC
conflict still answers 409 `GC conflict; retry` on the Silo lane, while a
destination collision answers 409 `Destination exists…`. They want opposite
handling — retry versus rename — and are distinguishable only by body text.
Moving GC conflict to 503 would resolve it cleanly and is a one-line change in
`writeCommitErr`, but it is a shipped status code that a client may key on, so
it is written down here rather than changed quietly. `porter-brief.md` now warns
about it.

**The newline-in-filename 404** noted at the end of this report is untouched;
it is a separate question about request parsing.

### Tests

`fileserver/write_contention_test.go`.
`TestConcurrentWritesReportContentionNotFailure` seeds a real library and races
eight writers at one head with the
retry budget set to zero, then asserts every loser comes back as
`ErrRetriesExhausted` and that `writeCommitErr` answers each one 503 rather than
500 — the whole chain, not just the mapping. `TestWriteCommitErr` covers the
four classes including a genuine failure, which must still read as one or the
fix has only moved the lie. `TestContentionBackoffIsBounded` samples the jitter
against the ceiling, because a bound that holds on average is not a bound.
