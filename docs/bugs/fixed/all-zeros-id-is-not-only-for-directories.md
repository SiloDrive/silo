# The all-zeros id is documented as the empty-*directory* sentinel; empty files share it

**Found:** 17 Aug 2026, against 0.4.1, re-verified against 0.4.2 on 18 Aug 2026.
**Status: fixed in 0.4.4** — a documentation defect, not a code one. The
server's behaviour was right and is unchanged; the brief described it too
narrowly. `docs/porter-brief.md` now carries the suggested wording, extended
with the reason (ids are content hashes, and empty content hashes to one value)
so the next reader can see why a second sentinel would be worse, rather than
only being told not to ask.
**Severity:** low, with a nasty failure mode — a client that acts on the
sentence as written mistakes empty files for directories.

## The sentence

`docs/porter-brief.md:170`:

> The all-zeros id is the empty-directory sentinel, not an error.

It is introduced immediately after a listing whose only all-zeros entry is a
`"type":"dir"`, so the reading it invites is "all-zeros means directory".

## What actually happens

The id is the id of *nothing at all*, and an empty file is also nothing.
Against 0.4.2:

```
$ curl -X PUT …/entries/zero-byte-probe.txt --data-binary ''
{"id":"0000000000000000000000000000000000000000","name":"zero-byte-probe.txt","size":0,"type":"file"}
```

and it lists the same way:

```json
{"name":"zero-byte-probe.txt","type":"file","id":"0000…0000","mtime":1787054316,"modifier":"test@test.com"}
```

So an empty file and an empty directory are indistinguishable by id. They are
perfectly distinguishable by `type`, which is the field that was always meant
to carry that.

## Why it is worth a line of prose

The obvious wrong move is a client that shortcuts on the id:

```go
if entry.ID == emptyID { /* it's an empty directory, no need to fetch */ }
```

which silently turns every zero-byte file in the account into a directory. It
is the sort of shortcut a client writes precisely *because* the brief named the
value after directories — the reader is told what the value means, and takes it
at its word rather than checking `type`.

The second-order hazard is caching. A content-addressed cache keyed on the id
would collide every empty object in the account onto one entry. Porter avoids
it by never fetching a zero-length entry at all, so it never keys anything on
the sentinel — but that is Porter's accident, not a property of the protocol,
and a client that streams unconditionally would have to notice this on its own.

## Suggested wording

> The all-zeros id is the id of an empty object, not an error. Both an empty
> directory and a zero-byte file carry it, so it says nothing about which one
> you have — read `type` for that, and do not use the id to tell them apart or
> as a cache key.

## Not a code change

Returning a distinct id for empty files would be worse: the id is a content
hash, empty content hashes to one value, and inventing a second sentinel to
make two nothings distinguishable would break the property that makes
content-addressing useful. The behaviour is correct. Only the sentence needs
widening.
