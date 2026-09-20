# Silo documentation

Every file here is one of three things, and confusing them is the expensive
mistake — a plan read as a description sends someone looking for code that was
never written, and a description read as a plan gets reimplemented.

- **current** — describes the server as it is. If the code and the document
  disagree, one of them is a bug.
- **plan** — where we are going. Nothing in it is built unless it says so, and
  the ones that are partly built say which part.
- **record** — done, superseded, or decided. Kept because the reasoning is why
  the code looks the way it does.

## One word for the thing an account owns

It is a **library**. Not a repo, not a repository, not a mount — a library, in
the code, in the schema, on the wire, in the CLI and in every document here.

It was both for a long time. The prose in `fileserver/api/api.go` said library
in every sentence while the struct beside the sentence was called `Repo` and
the route under it was `/repos`, which is the upstream inheritance showing
through. That is settled: `/api/silo/v1/libraries/{libraryid}`, `library_id` in
every payload, `Library*` tables, and `silo library create` on the CLI.

Two consequences worth knowing before you name something:

- **The manager package is `libmgr`**, not `librarymgr`, matching `objmgr` and
  `authmgr`. The abbreviation is house style for a manager package and is
  deliberate rather than an oversight.
- **The rename is finished, and the guard that enforced it is gone.**
  `internal/lexicon` used to scan the whole tree for the old word. It did its
  job; what it became afterwards was a tax, because every remaining hit was a
  false one — the git sense of the word, or a bug report naming a route by the
  spelling it had when it broke — and each cost an allowlist entry arguing why
  a true sentence was allowed to stay true. What survives is the word itself,
  named once, so the wire and CLI tests can assert the old spellings answer 404
  without writing them out by hand.

The migration note for the rename is
[`upgrading-to-0.5.0.md`](upgrading-to-0.5.0.md), which writes the old word out
because a client author has to grep for it.

## Start here

| if you are | read |
|---|---|
| running a server | the [`README`](../README.md), then [`backup.md`](backup.md) |
| writing a client | [`protocol.md`](protocol.md) → [`responses.md`](responses.md) |
| changing the server | [`notes.md`](notes.md), then [`storage.md`](storage.md) and [`responses.md`](responses.md) for what binds a change |
| choosing a status code | [`responses.md`](responses.md). Before writing the handler, not after |
| deciding what to build next | [`roadmap.md`](roadmap.md), then the plans below |

## The server as it is

| file | what it covers |
|---|---|
| [`protocol.md`](protocol.md) | **current.** Every HTTP endpoint, plus what is deliberately not implemented, plus — under *Writing a client* — the guidance a client author needs that the endpoint table does not say: quota arithmetic, the `412` inside `modifyItem`, the E2EE client shape, `.history/`, the request inventory. The single contract with the clients |
| [`responses.md`](responses.md) | **current.** What each status code means, and which are already spoken for. The registry that stops two handlers giving one code opposite meanings |
| [`notes.md`](notes.md) | **current.** Architecture: where the code came from, what the packages are, the storage layout, the data model |
| [`backup.md`](backup.md) | **current.** Why `cp silo.db` is not a backup, the order the two halves are captured in, restore and verification |
| [`error-reporting.md`](error-reporting.md) | **current.** Sentry-compatible error reporting: what is sent, how issues group, how to turn it off |
| [`storage.md`](storage.md) | **current and plan, in one file, split down the middle.** Part 1 is the store as it runs — objects, keyed content-defined chunking, what the server can read of a library it holds no key for, the numbers and what reclaims them — and is normative. Part 2 is designed and not built: packs, durable tiers and their attributes, and compaction. Byte layouts are not here; they are `spec/store-format.md` |
| [`../test/README.md`](../test/README.md) | **current.** The Ruby integration harness that runs against a live server |

## Plans — where we are going

| file | what it covers |
|---|---|
| [`auth.md`](auth.md) | **current and plan, in one file, split down the middle.** Part 1 describes the credential model as it runs — the token format, the one `Resolve`, the permission ceiling, password enrolment — and is normative: if the code disagrees with it, one of them is a bug. Part 2 is designed and not built — proof of possession, Argon2id, OIDC — and ends with what is left, in order. The account key material and the single-use setup token it used to list as unbuilt have both landed |
| [`roadmap.md`](roadmap.md) | **plan.** What is built, what is next, and what each thing waits on. It owns **ordering and dependency only** and links to the document that owns each design — where it and an owning document disagree, the owning document wins |
| [`protocol-gaps.md`](protocol-gaps.md) | **plan.** What the Silo lane still lacks for a Dropbox-shaped client — a delta for wholesale-rewritten files, stable per-file identity, search — ranked, with the reasoning attached. What exists is in `protocol.md`, not here |
| [`encryption.md`](encryption.md) | **plan.** Why the inherited encrypted-library format was not adopted, and the sketch of the end-to-end scheme that replaces it. Also the list of things not to build |
| [`protocol-frontends.md`](protocol-frontends.md) | **plan.** A survey of what else could front the same store — WebDAV, S3, SFTP — and what each costs |
| [`spec/store-format.md`](spec/store-format.md) | **specification, complete.** The normative byte-level contract between the Go `store/` package and silo-drive's Swift port — ids, chunking, the manifest/directory/commit codecs, the content crypto, AES-SIV names and key wrapping. Test vectors live beside it in `store/testdata/vectors`; a port is conforming when it reproduces them |
| [`plans/sharing.md`](plans/sharing.md) | **plan.** Accounts, roles and invites, public read-only libraries, and share links — including links out of E2EE libraries, which wrap keys rather than re-encrypting content |
| [`target.md`](target.md) | **plan.** The end state Silo is aiming at — no steps, no dates. Where `roadmap.md` is the route, this is the destination |
| [`plans/credential-management.md`](plans/credential-management.md) | **plan.** Listing and revoking the credentials an account holds, without an administrator and without signing everything out. Why the password change revokes everything today, and what has to exist before that can be reconsidered |
| [`plans/events.md`](plans/events.md) | **plan.** An append-only log for the control plane and access, for the questions the schema forgets |
| [`plans/e2ee-completion.md`](plans/e2ee-completion.md) | **plan.** From a server that can store E2EE to a person who can use it: the sealing client, split-derivation login, sharing, silo-drive — with the issue that tracks each |
| [`plans/locking.md`](plans/locking.md) | **plan, parked.** Per-file advisory locks, and why they should not ship without the push event |
| [`plans/search.md`](plans/search.md) | **plan, parked.** A filename index on SQLite FTS5; populating it is the hard part, not querying it |
| [`plans/replication.md`](plans/replication.md) | **plan, parked.** Primary plus read-only secondaries, what a secondary needs beyond bytes, and where parity would fit |
| [`plans/distributable-library.md`](plans/distributable-library.md) | **plan, parked.** A plain library as a casync-shaped read-only store: a signed head, an export, a verifying consumer |
| [`quota.md`](quota.md) | **plan, mostly landed.** Whose ceiling, what it counts, and when the things it counts stop counting. Enforcement, the CLI, history expiry and orphan collection are built; charging chunks rather than logical size and a server-wide ceiling are argued here and not written |

## Records — decided, done, or superseded

| file | what it covers |
|---|---|
| [`chunking.md`](chunking.md) | **record.** Why chunk boundaries are content-defined, what the store-v2 migration settled, and the two read paths a client chooses between |
| [`plans/hash-choice.md`](plans/hash-choice.md) | **record.** The hash benchmark on arm64 and x86, and why SHA-256 won a race BLAKE3 leads on one of the two machines |
| [`capability-urls.md`](capability-urls.md) | **record.** Why the Silo lane stopped redirecting to `/files/{token}/`, and what signed URLs should mean if they are ever wanted |
| [`bugs/`](bugs/README.md) | **record.** One file per bug. Open reports sit in `bugs/`, fixed ones in `bugs/fixed/` with what was done. Several are cited from comments in the code |
| [`feature-req/`](feature-req/) | **record.** Requests from client authors. The one there landed in 0.4.4 |

## Where a new document goes

`plans/` is for a plan scoped to one change — what to write, where, and how to
verify it. `docs/` holds the standing documents: the descriptions above, and the
long-lived designs like [`auth.md`](auth.md), [`storage.md`](storage.md) and
[`encryption.md`](encryption.md)
that a dozen other files cite and that outlive any one implementation of them.
A bug report goes in `bugs/`, named for the symptom rather than the cause.

When a plan lands, it does not move — it gains a section saying what shipped
and how that differed from the proposal, and its row here changes. That is
cheaper than a migration of links, and the reasoning stays next to the decision
it produced.
