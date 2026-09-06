# Moving a file onto a directory destroyed the directory

A move whose destination was an existing directory replaced that directory with
the moved file and orphaned everything under it — on a `200`, with nothing
logged and nothing for the caller to notice.

**Status: fixed in v0.4.3, 2026-08-18** — `fileserver/api_handlers.go` and
`fileserver/api_handlers_test.go`.

Found while capturing the `/changes` wire contract for silo-drive's M3, by moving
things around a scratch library to see what the delta endpoint would say about
them. It is not a `/changes` bug — that endpoint reported the damage accurately.

## Reproduction

Against 0.4.2, in any library:

```
silo mkdir  $LIBRARY /Precious
silo put    $LIBRARY ./keep.txt /Precious
silo put    $LIBRARY ./junk.txt /
silo mv     $LIBRARY /junk.txt /Precious      # exit 0
silo get    $LIBRARY /Precious/keep.txt       # 404 Not Found
```

`/Precious` is now a 5-byte file. `keep.txt` is unreachable, as is anything else
that was under there, however deep.

Over the wire that is `POST /api/silo/v1/libraries/{library}/entries/junk.txt` with
`{"op":"move","to":"/Precious"}`. `postEntry` delegates to `moveHandler`, so the
same hole is reachable from the older `?src=&dst=` form.

The objects themselves survive — storage is content-addressed and older commits
still reference them — so this is recoverable from history. It is unreachable
through the API, which for a sync client is the same thing.

## Root cause

`moveHandler` is add-to-destination then delete-from-source, and phase 1 ran with
replace enabled:

```go
// Shape as it stood in 0.4.2. The tree layer has since been rewritten and
// the package these types came from no longer exists; the argument that
// mattered is the one marked below.
newDent := NewDirent(srcEntry.ID, dstName, srcEntry.Mode, ...)
rootAfterAdd, err := DoPostMultiFiles(library, head.RootID, dstDir,
    []*Dirent{newDent}, user, true, &names)
                              // ^^^^ replace existing
```

Nothing between parsing the request and that call looked at the destination. The
handler already guarded the *other* way a move can eat a tree — `movesIntoOwnSubtree`,
which rejects relocating a directory beneath itself — but that guard is about the
source's relationship to the destination path, not about what is sitting at that
path already. A directory dirent and a file dirent are the same shape in the
parent's entry list, so replacing one with the other is a legal edit to the tree
and the whole subtree simply stops being referenced.

## The fix

A `destructiveCollision` predicate and a guard before phase 1, deliberately the
same shape and placement as `movesIntoOwnSubtree`:

| source | destination | result |
|---|---|---|
| any | absent | proceeds |
| file | file | proceeds — replaces |
| file | directory | **409** `Destination exists and is a directory` |
| directory | directory | **409** `Destination exists and is a directory` |
| directory | file | **409** `Destination exists and is a file` |

`409` because silo-drive's error table already maps it to
`NSFileProviderError.filenameCollision` — "return the existing item so the system
renames" — so the File Provider client behaves sensibly without further work.

**File-onto-file still replaces, on purpose.** It is the one collision that
destroys nothing the caller did not name, and `PUT entries/{path}` already
replaces rather than renaming the collision; a move should not be stricter than a
write. If we ever want every collision to `409`, that is a contract change to
make in both places at once, not a quiet asymmetry between them.

## Tests

- `TestDestructiveCollision` — the table above.
- `TestMoveOntoDirectoryWouldDestroyIt` — builds the tree and runs both move
  phases directly, asserting the destruction happens, then asserting the guard
  rejects it. Modelled on `TestMoveIntoOwnSubtreeWouldDestroyIt`, and for the
  same reason: a guard whose test only checks the guard will not notice when the
  algorithm underneath it changes shape.

## Related, and not a bug: `/changes` can report two ops for one path

The delta endpoint described the damage honestly, and the shape it used is a trap
for clients:

```json
{"op": "move",   "path": "/Doomed", "old_path": "/junk.txt", "is_dir": false}
{"op": "delete", "path": "/Doomed/keep.txt",                 "is_dir": false}
{"op": "delete", "path": "/Doomed",                          "is_dir": true}
```

One batch, one path, both created and deleted — distinguished only by `is_dir`.
`/changes` computes a net diff between two trees rather than replaying a log,
which is the right design, but it means a client keyed on path alone will apply
those in some order and can delete the file it just created.

This survives the fix, and not hypothetically — the move above is now refused,
but deleting a directory and creating a file of the same name between two anchors
reaches the identical shape by a route no guard touches:

```
silo mkdir $LIBRARY /Z ; silo put $LIBRARY ./keep.txt /Z     # anchor taken here
silo rm    $LIBRARY /Z
silo put   $LIBRARY ./z.txt / ; silo rename $LIBRARY /z.txt Z
```

```json
{"op": "create", "path": "/Z",          "is_dir": false, "size": 11}
{"op": "delete", "path": "/Z/keep.txt", "is_dir": false}
{"op": "delete", "path": "/Z",          "is_dir": true}
```

Any client consuming `/changes` has to treat
`(path, is_dir)` as the key, or carry stable identifiers of its own — which is
the argument for silo-drive's `IdMap` landing with M3 rather than being deferred to
M4.
