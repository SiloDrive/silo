# objmgr / objstore — review findings, 2026-08-23

Four cleanup agents reviewed `e201ead..cf7a920` (reuse, simplification,
efficiency, altitude). The `store/` findings were applied in `ef3d2bf` and
`5d40ee3`. These are the ones in `fileserver/objmgr/` and
`fileserver/objstore/`, which were being actively written at the time and were
left alone rather than edited underneath.

Nothing here is a correctness bug — that was explicitly out of scope. Line
numbers are as of `cf7a920` and will have moved.

Every measured claim below is reproducible; the method is given so this can be
re-verified rather than trusted.

---

## 1. Every stored byte is SHA-256'd twice on the write path

`objmgr.go` — `putEncoded`, `putChunkData`.

`putChunkData` computes the id from the in-memory buffer — `store.ChunkID(data)`,
or `store.SealChunk` which returns `sealed.ID = ChunkID(frame)` — then hands the
*same buffer* to `PutChunk` → `WriteVerified`, which tees it through a fresh
`sha256` and hex-compares the digest against the id it was just given.
`putEncoded` does the same via `store.ObjectID(encoded)` → `PutObject`. One
buffer, one goroutine, microseconds apart, nothing untrusted in between.

Measured, 1 MiB chunk, warm cache:

```
WriteVerified (today)     1,391,470 ns/op    753 MB/s   36,869 B/op   37 allocs/op
Write (id already known)    860,280 ns/op   1219 MB/s    3,796 B/op   31 allocs/op
```

**38% of the chunk write path**, plus ~33 KB of garbage per chunk — the
`io.TeeReader` wrapper also defeats `io.Copy`'s `WriteTo` fast path, forcing a
32 KiB staging buffer a bare `*bytes.Reader` would not need. At 16 MiB through
`WriteFile` the redundant pass alone is ~16.5 ms of a ~41 ms total.

**How to re-verify:** write a benchmark in `fileserver/objstore` that calls
`WriteVerified` and `Write` with the same 1 MiB buffer and a precomputed id,
`b.SetBytes(1<<20)`, `-benchmem`.

**Suggested:** have `putEncoded`/`putChunkData` call `s.chunks.Write` /
`s.objects.Write`, and keep `WriteVerified` for the exported `PutChunk`/
`PutObject` — the ingest paths for client-supplied bytes. That is the
distinction `WriteVerified`'s own doc comment already draws; the internal
callers just sit on the wrong side of it.

---

## 2. `WriteFile` retains 64 KiB per inline manifest

`objmgr.go` — `WriteFile`, the `head := make([]byte, store.InlineThreshold)`
allocation and the `Inline: head` that follows `head = head[:n]`.

The 64 KiB backing array stays reachable for the manifest's whole lifetime.
Writing a directory of small files — the real ingest shape, and the shape
`buildTree` tests — holds 64 KiB per manifest instead of `n` bytes. For 10k
inline files that is ~640 MB resident against a few MB.

**How to re-verify:** `runtime.ReadMemStats` around a loop that `WriteFile`s
10k small files and keeps the returned manifests alive; compare with
`bytes.Clone` applied.

**Suggested:** `Inline: bytes.Clone(head)`. One word.

(The per-call 64 KiB allocation-and-zero on *every* `WriteFile`, including a
9-byte file, is a separate and much smaller matter — ~2-3 µs. A `sync.Pool`
would fix it but adds machinery for a modest win.)

---

## 3. `GetChunk`/`GetObject` read into a buffer that doubles from 512 bytes

`objmgr.go` — `GetChunk`, `GetObject`.

`var buf bytes.Buffer` + `ObjectStore.Read` → `io.Copy` → `Buffer.ReadFrom`,
which grows from `MinRead` by doubling. Chunks are 256 KiB–4 MiB
(`store.DefaultMinSize`/`DefaultMaxSize`), so a full-size chunk costs ~12
reallocate-and-copy steps. Measured on a 4 MiB chunk:

```
GetChunk (growing buffer)   2,058,380 ns/op   16,777,537 B/op   23 allocs/op
sized read via ReadAt         444,792 ns/op    4,195,064 B/op    8 allocs/op
```

**4.6× slower and 4× the allocation**, per chunk, on `ReadFile` — the per-chunk
download path for every file.

**How to re-verify:** benchmark `GetChunk` against `make([]byte, n)` +
`chunks.ReadAt(..., p, 0)` on a 4 MiB stored chunk, `-benchmem`.

**Suggested:** an internal `getChunkSized(id, n)` called from `ReadFile`, which
already has `ref.Size` (plus `store.TagSize` under E2EE). Keep the current
`GetChunk` for callers with no size.

---

## 4. The three-state dispatch is written twelve times

`objmgr.go` — `encodeManifest`, `GetManifest`, `PutDirectory`, `GetDirectory`,
`PutCommit`, `GetCommit`; plus the `s.e2ee && !s.HasKey()` guards in
`objmgr.go` and `tree.go`.

Six copies of `!e2ee → plain codec / HasKey → sealed codec / else →
ErrNoContentKey`, in two different shapes (an if-chain in `encodeManifest`, an
inline `switch` with a `var encoded []byte; var err error` preamble elsewhere),
plus six further guards that re-derive the same three states.

Three of the four agents raised this independently. It matters more than a
usual dedup because "three states, not two" is the package doc's headline
claim: a seventh object type is six more edits with no compiler help, and the
one that gets missed is the one that half-works.

**Suggested:** `func (s *Store) encodeFor(plain func() ([]byte, error), sealed
func([]byte) ([]byte, error)) ([]byte, error)`, a package-level
`decodeFor[T any](s *Store, b []byte, ...)` (methods can't be generic, a free
function can), and `func (s *Store) requireKey() error` for the guards.

Related, same mechanism: `List` and `PutDir` in `tree.go` guard on
`s.e2ee && !s.HasKey()` and then call `GetDirectory`/`PutDirectory`, which
apply the identical guard; `Walk` adds a third layer through `walk` → `List`.
`Resolve`'s guard does earn its place — an empty path returns before any fetch.

---

## 5. `bothTypes` exists and is inlined five more times

`objmgr_test.go` — five or six sites open with the same eight lines:

```go
for _, tc := range []struct{ name string; build func(*testing.T) *Store }{
    {"plain", plainStore}, {"sealed", sealedStore},
} { t.Run(tc.name, func(t *testing.T) { ... }) }
```

`bothTypes` in `tree_test.go` — same package, introduced in the same diff — is
exactly that, and `tree_test.go` uses it throughout. ~48 lines, and the two
files read as if they had different conventions.

---

## 6. Rules restated instead of asked

- **`SplitPath` (`tree.go`) hand-checks `"."`/`".."`** and constructs
  `store.ErrName` by hand. `store.ValidName` already refuses empty, `/`, NUL,
  `.` and `..` and already returns that sentinel. The copy covers less: a
  segment containing a NUL passes `SplitPath` today. Worse, the lanes now
  disagree — the E2EE lookup path gets the full rule free because
  `dirReader.lookup` calls `NameCipher.Encrypt` → `ValidName`, while the plain
  lane does `want := []byte(name)` and validates nothing. `ValidName` gaining a
  case would silently apply to E2EE resolves and not plain ones.
- **`WriteFile` tests `n < store.InlineThreshold`** where `store.Inlined(int64(n))`
  is the format's answer to that exact question — and `ReadFile` in the same
  file already calls `store.Inlined`, so the file is inconsistent with itself.
  `Manifest.Inlined`'s doc argues the rule must have one home because "two
  writers disagreeing about a 30 KB file would mint two manifest ids".
- **The name-cipher construction is written twice** (`openDir` and `PutDir` in
  `tree.go`): `store.NameKey(s.ck, salt)` → `store.NewNameCipher(key)` with two
  error returns, ~9 lines each. One `func (s *Store) nameCipher(salt) (*store.NameCipher, error)`
  returning `nil, nil` when `!s.e2ee` collapses both to two lines.
- **`PutChunk`/`PutObject`, `GetChunk`/`GetObject`, `HasChunk`/`HasObject`** are
  pairwise identical apart from `s.chunks` vs `s.objects` and one word in the
  error.

---

## 7. Seam-depth points

These are not cleanups; they are about where a rule lives.

- **`validPackID` guards one backend, not the seam.** It lives in
  `backend_fs.go` and is called from two places in that file.
  `ObjectStore.Read/ReadAt/Write/WriteVerified/Stat/List/Remove/RemoveLibrary` all
  pass the id straight through, and the `storageBackend` interface doc — which
  is otherwise explicit about what the contract is and is not — says nothing
  about id validation. The package doc says phases 4 and 6 add more
  implementations; each has to remember to re-derive the rule.
  `verifierFor` is the other half of the same policy and is *already* called
  from the seam while living in the backend file — and its comment claims
  "validPackID has already established that the width is one of the two", which
  is not true at that call site, since `WriteVerified` picks the digest before
  any backend runs.
- **`libraryID` is validated nowhere**, and `removeLibrary` is
  `os.RemoveAll(path.Join(b.objDir, libraryID))`. The seam validates one
  identifier and not its sibling, on the one verb where the difference is
  destructive.
- **`validPackID` is also a near-duplicate of `utils.IsObjectIDValid`**
  (`fileserver/utils/utils.go`), which is 40-hex-only and live at 13 handler
  sites. Two homes for the hex rule that can disagree about what an id may
  spell. Importing `utils` here causes no cycle — it only pulls in `option`,
  `jwt`, `uuid`.
- **`objstore.New`'s deferred-error workaround is now inherited by a caller
  that has an error return.** Its doc justifies stashing init failure in
  `initErr` because "the three managers that call this have Init functions
  returning nothing … they are deleted by the end of the store cutover".
  `objmgr.New` is a fourth caller, brand new, and it *does* return
  `(*Store, error)` — but can't ask, because `ready()`/`initErr` are
  unexported. So it returns a healthy-looking `Store` on an unusable data
  directory and the failure surfaces at the first `PutChunk`. A compromise made
  for code with a known end date has become the permanent shape for the code
  replacing it. Suggest an exported `Err()`.
- **`ErrIDMismatch` discards `objstore.ErrContentMismatch`.**
  `fmt.Errorf("%w: chunk %s", ErrIDMismatch, id)` drops the inner sentinel, so
  `errors.Is(err, objstore.ErrContentMismatch)` stops working across the
  boundary — and that sentinel is exported precisely to be tested for.
  `fmt.Errorf("%w: chunk %s: %w", ErrIDMismatch, id, err)`.

---

## 8. Smaller

- `ObjectStore.ready()` is a method that returns a field, called at eight sites.
- `objstore.New` still takes a config-path argument but ignores it in the body — the
  new `objmgr` callers passing `""` are the tell.
- `ObjectStore.ObjType` has no reader anywhere.
- The `ObjType` field comment and `New`'s doc still say "one of TypeCommits,
  TypeFS or TypeBlocks" now that `TypeChunks` and `TypeObjects` exist and are
  in `Types`.
- `Exists` was reimplemented over `Stat`, so every absent object now allocates
  a formatted error that `Exists` immediately discards via `errors.Is`. The
  deleted `fsBackend.exists` returned `(false, nil)` allocation-free.
  `/check-blocks` loops over a client-supplied id list where misses are the
  expected answer. ~2 allocations and ~150-250 ns against a ~1-2 µs `stat`, so
  roughly 10% of that loop — real but small. A `notFoundError` struct
  formatting lazily with `Unwrap() error { return ErrNotFound }` keeps the
  seam's context for free.
- `objmgr.New` builds two `ObjectStore`s per call and each `newFSBackend` does
  two `MkdirAll`s, one of them on the identical `tmpDir` path — four syscalls
  per `Store`. The two depend only on `DataDir`, so they could be built once
  per data directory. Nothing constructs a `Store` in production yet, so this
  is cheap to fix now and stops the count scaling with request rate later.
- `testData` regenerates 3 MiB from scratch ~20 times: 8.0 ms per call, ~160 ms
  of the package's ~490 ms. A package-level `sync.OnceValue` per (label, size)
  recovers most of it. (Hoisting the `append` inside the loop buys nothing —
  the compiler already stack-allocates it.)

---

## Deliberate follow-ups, not cleanups

- **`gc.go`'s `measure` and `reclaim` now duplicate the seam.** `measure` walks
  the fan-out with `objstore.LibraryDir` + `filepath.WalkDir` and `reclaim` deletes
  with `os.RemoveAll` — which is what `ObjectStore.List` and
  `RemoveLibrary` now do behind the interface. Two walkers of the on-disk layout,
  and the S3 backend the seam exists for will only fix one of them. Behaviour
  differs by design — `list` skips temp-file debris that `measure` counts — so
  this is a decision, not a mechanical swap.
- **Concurrent chunk fetches in `ReadFile`.** The fetches are independent; only
  the writes are ordered. On local disk a read-ahead buys little and is not
  worth doing now — but phase 6 puts a durable tier behind that loop, where
  serialised round-trips dominate and it stops being optional.

---

## Checked and cleared

Recorded so nobody re-litigates them:

- `readAt`/`list`/`remove` having no callers — documented as phase 4/5 staging
  in the plan and the package doc.
- `Store.e2ee` alongside `ck` — not derivable; the third state is the point.
- `dirReader` builds the name cipher once per directory and `lookup` encrypts
  the wanted segment rather than decrypting every entry. Both the right way
  round.
- `backend_fs.list` uses `DirEntry.Info()` rather than a second `stat`.
- No new argon2id derivations were added to any test.
