# `GET /repos` answers `null` for an empty account, where `/changes` answers `[]`

**Found:** 18 Aug 2026, against 0.4.1, re-checked against 0.4.3 (`5cd182a`).
**Status: fixed in 0.4.4.** `scanRepos` allocates.
**Severity:** low, and permanently cheap to fix. It costs nothing today because
the only client is written in Go; it costs an afternoon the first time one is
not.

Two list endpoints on the same lane disagree about how they spell "nothing".

## Symptom

```
GET /api/silo/v1/repos            → null
GET /api/silo/v1/repos/{id}/changes?since=…   → {"anchor":"…","changes":[]}
```

`porter-brief.md` is explicit about the second: *changes is always an array,
never null*. Nothing says anything about the first, and it does the opposite.

## Cause

`scanRepos` (`fileserver/api/api.go:229`) opens with

```go
var repos []repoInfo
```

which is nil when the loop never runs. `ListReposHandler` appends shared repos
to it and hands it to `writeJSON`, and `encoding/json` renders a nil slice as
`null`. `ChangesHandler` does not have the bug because
`fileserver/api/changes.go:124` allocates:

```go
changes := make([]change, 0, len(entries))
```

So the fix is one word, in one of two places — either

```go
repos := make([]repoInfo, 0)
```

in `scanRepos`, or the same normalisation in `ListReposHandler` before the
write. `scanRepos` is the better home: it is also called for the shared-repo
query, so fixing it there fixes the source rather than the symptom.

## Why it has not bitten

Go decodes `null` into a nil slice without complaint, and a nil slice ranges
zero times, so Porter has never noticed. `len()` on it is 0 and `append` to it
works. The language makes the two spellings indistinguishable at the point of
use.

That is exactly what makes it worth fixing before it matters. A client in
TypeScript, Python, Swift or Rust does distinguish them:

```js
(await res.json()).map(…)      // TypeError: null is not iterable
```

```python
for repo in res.json():        # TypeError: 'NoneType' object is not iterable
```

The macOS File Provider extension in `macos-fileprovider-plan.md` is Swift, and
`[Repo]?` versus `[Repo]` is a decoding decision it would have to make on the
strength of an undocumented behaviour.

## What makes it a bug rather than a preference

Not that `null` is wrong in itself — plenty of APIs use it. It is that the two
endpoints differ, one of them is documented as the opposite of what the other
does, and a client has no way to learn which is which except by emptying an
account and looking. An account with no libraries is also the state every new
account is in, so this is the *first* response a fresh client sees.

## Reproduction

Needs an account owning no libraries and party to no shares, which the current
`SILO_ADMIN_EMAIL` bootstrap does not readily produce — one reason this was
noticed by reading rather than by running. The code path is unambiguous, and
`scanRepos` returning nil on an empty result set can be asserted directly in a
unit test without standing up an account at all.

## What was done

`scanRepos` (`fileserver/api/api.go`) now opens with `repos := make([]repoInfo, 0)`,
which was the report's preferred of the two homes: it is also called for the
shared-repo query, so both paths are fixed at the source. `ListReposHandler`
appends to what it returns, so the handler cannot reintroduce nil.

`TestListReposAnswersEmptyArrayNotNull` asserts on the response bytes rather
than the decoded value — decoding into `[]map[string]any` is exactly the thing
that cannot tell the two spellings apart, so a test written that way would pass
against the bug. It also checks the populated case still lists, so the fix is
not just an endpoint that returns nothing.
