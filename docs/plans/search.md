# Plan: search (a filename index)

**Status: not built, and parked** until a client actually needs it.

Motivated by the macOS File Provider's search pushdown
(`NSFileProviderSearching`, macOS 26+ — the porter-macos repository has the
plan), but useful on its own independent of any one client.

Moved here from `future-features.md` when that file was replaced by
[`roadmap.md`](../roadmap.md), which holds ordering rather than designs.

## What exists today: nothing

Silo has **no search of any kind**. Files live as content-addressed objects
inside per-library commit trees, not as rows in a table, so there is nothing to
`WHERE filename LIKE` against — finding a file means the *client* walking the
current tree itself.

Upstream's paid edition answered this with an external Elasticsearch-backed indexer
(a separate indexing daemon). That was commercial-only, and an operational dependency this
project should not force on a single self-hosted binary.

## Endpoints

- `GET /api/silo/v1/search?q=…` — account-wide, filtered to libraries the
  caller can see.
- `GET /api/silo/v1/libraries/{id}/search?q=…` — a scoped variant, lower
  priority.

## Design

**SQLite FTS5, not Elasticsearch.** The database is already there; FTS5 gives
prefix and substring matching on filenames with no new operational dependency.
Filenames only for v1 — content search means extracting and indexing blob text,
a much bigger lift, and not needed to match what Spotlight and the Finder
actually ask for.

**Populating the index is the hard part, not querying it.** There is no
existing "list every filename in a library" call to seed from, so it needs a
full tree walk once and then incremental maintenance per commit. Reuse the
commit-diffing machinery already backing the change feed
(`fileserver/api/changes.go`, `fileserver/diff/diff.go`) rather than re-walking
whole trees on every write.

**Permission-filtered at query time**, not through separate per-grantee
indexes — join against the same visibility check the listing already performs,
so a share revoked mid-session cannot leak stale results.

Ranking is prefix and substring, plus perhaps recency. Nothing at this scale
needs real relevance scoring.

## An E2EE library cannot be in it

Names are ciphertext under a key the server does not hold, which the rest of
the design has to state rather than discover: an encrypted library is absent
from server-side results, and a client that wants search across one has to do
it itself over names it can decrypt. This is the same shape as every other
plaintext-requiring feature and belongs scoped to plain libraries explicitly.

## Open questions

- One FTS table across all libraries, join-filtered per query, or one per
  library. All-library is simpler to query; per-library is simpler to rebuild
  in isolation and to scope alongside quota and GC.
- Whether virtual libraries — subdirectory shares — need the special-casing
  that `checkQuota` already does for them.
- Backfill cost on an existing large library. The first build is a full tree
  walk and should run as a background job rather than inline on first query.
