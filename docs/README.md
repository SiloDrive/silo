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
the route under it was `/repos`, which is the Seafile inheritance showing
through. That is settled: `/api/silo/v1/libraries/{libraryid}`, `library_id` in
every payload, `Library*` tables, and `silo library create` on the CLI.

Two consequences worth knowing before you name something:

- **The manager package is `libmgr`**, not `librarymgr`, matching `objmgr` and
  `authmgr`. The abbreviation is house style for a manager package and is
  deliberate rather than an oversight.
- **`internal/lexicon` enforces this with a test.** Nothing breaks when a
  variable is called `repoID`, which is exactly why nothing else catches it. If
  the guard fails, the fix is the name, not the guard — except for another
  product's names, which are exempt by token: Seafile shipped
  `Seafile-Repo-Token` and an `/api2/repos` lane, and rewriting those would
  document a header that never existed.

The migration note for the rename is
[`upgrading-to-0.5.0.md`](upgrading-to-0.5.0.md) — the only other document here
allowed to write the old word out, because a client author has to grep for it.

## Start here

| if you are | read |
|---|---|
| running a server | the [`README`](../README.md), then [`backup.md`](backup.md) |
| writing a client | [`protocol.md`](protocol.md) → [`porter-brief.md`](porter-brief.md) → [`responses.md`](responses.md) |
| changing the server | [`notes.md`](notes.md), then [`plan.md`](plan.md) for the constraints that bind every change |
| choosing a status code | [`responses.md`](responses.md). Before writing the handler, not after |
| deciding what to build next | [`future-features.md`](future-features.md), then the plans below |

## The server as it is

| file | what it covers |
|---|---|
| [`protocol.md`](protocol.md) | **current.** Every HTTP endpoint, grouped by lane, plus what is deliberately not implemented. The contract with the clients |
| [`responses.md`](responses.md) | **current.** What each status code means, and which are already spoken for. The registry that stops two handlers giving one code opposite meanings |
| [`porter-brief.md`](porter-brief.md) | **current.** The wire contract for a native client, with responses captured from a running server |
| [`notes.md`](notes.md) | **current.** Architecture: where the code came from, what the packages are, the storage layout, the data model |
| [`backup.md`](backup.md) | **current.** Why `cp silo.db` is not a backup, the order the two halves are captured in, restore and verification |
| [`error-reporting.md`](error-reporting.md) | **current.** Sentry-compatible error reporting: what is sent, how issues group, how to turn it off |
| [`sync-design.md`](sync-design.md) | **current.** The reasoning behind the storage and sync model — the *why* behind `protocol.md` |
| [`../test/README.md`](../test/README.md) | **current.** The Ruby integration harness that runs against a live server |

## Plans — where we are going

| file | what it covers |
|---|---|
| [`auth.md`](auth.md) | **plan, partly landed.** One credential model to replace three stores, the identity split behind it, proof of possession, OIDC. The `Credential` and `Account` tables and `credential.Resolve` are built; the lanes are being moved onto them now, so read the order-of-work list at the end as the state of play |
| [`future-features.md`](future-features.md) | **plan.** The roadmap: user management, sharing, groups, locking, trash, quota, search. Loosely ordered, nothing committed |
| [`protocol-gaps.md`](protocol-gaps.md) | **plan.** What the Silo lane would need to be an ideal protocol for a Dropbox-shaped client, ranked, with the reasoning attached |
| [`encryption.md`](encryption.md) | **plan.** Why Seafile's encrypted libraries are not being adopted, and the sketch of the end-to-end scheme that replaces them. Also the list of things not to build |
| [`native-client.md`](native-client.md) | **plan, partly landed.** Three tiers between the CLI and a real sync agent; two have landed |
| [`macos-fileprovider-plan.md`](macos-fileprovider-plan.md) | **plan, server half landed.** The macOS File Provider client. For the wire contract as built, read `porter-brief.md` instead |
| [`compression.md`](compression.md) | **plan.** What is compressed today, why the format is swappable, and where the win actually is. Measure first |
| [`protocol-frontends.md`](protocol-frontends.md) | **plan.** A survey of what else could front the same store — WebDAV, S3, SFTP — and what each costs |
| [`spec/store-format.md`](spec/store-format.md) | **specification, complete.** The normative byte-level contract between the Go `store/` package and porter-mac's Swift port — ids, chunking, the manifest/directory/commit codecs, the content crypto, AES-SIV names and key wrapping. Test vectors live beside it in `store/testdata/vectors`; a port is conforming when it reproduces them |
| [`plans/store-v2.md`](plans/store-v2.md) | **plan, format closed, phase 1 done — phase 2 next.** The store that replaces Seafile's object formats: content-defined chunking, SHA-256 ids, manifests, packs, E2EE by default. The object layouts and key derivations are settled to the byte — build against them rather than reopening them |
| [`plans/sharing.md`](plans/sharing.md) | **plan.** Accounts, roles and invites, public read-only libraries, and share links — including links out of E2EE libraries, which wrap keys rather than re-encrypting content |
| [`target.md`](target.md) | **plan.** The end state Silo is aiming at — no steps, no dates. Where `future-features.md` is the route, this is the destination |
| [`plans/events.md`](plans/events.md) | **plan.** An append-only log for the control plane and access, for the questions the schema forgets |
| [`plans/admin-check.md`](plans/admin-check.md) | **plan.** The `is_staff` gate, which nothing yet consumes |

## Records — decided, done, or superseded

| file | what it covers |
|---|---|
| [`plan.md`](plan.md) | **record.** What replaced what when the C daemon and the Python layer went, plus the standing constraints every change still has to hold |
| [`plans/store-v2-hash-bench.md`](plans/store-v2-hash-bench.md) | **record.** Gate G2: the hash benchmark on arm64 and x86, and why SHA-256 won a race BLAKE3 leads on one of the two machines |
| [`capability-urls.md`](capability-urls.md) | **record.** Why the Silo lane stopped redirecting to `/files/{token}/`, and what signed URLs should mean if they are ever wanted |
| [`plans/db-rename.md`](plans/db-rename.md) | **record, superseded.** Proposed two renamed databases; one merged `silo.db` shipped instead |
| [`bugs/`](bugs/README.md) | **record.** One file per bug. Open reports sit in `bugs/`, fixed ones in `bugs/fixed/` with what was done. Several are cited from comments in the code |
| [`feature-req/`](feature-req/) | **record.** Requests from client authors. The one there landed in 0.4.4 |

## Where a new document goes

`plans/` is for a plan scoped to one change — what to write, where, and how to
verify it. `docs/` holds the standing documents: the descriptions above, and the
long-lived designs like [`auth.md`](auth.md) and [`encryption.md`](encryption.md)
that a dozen other files cite and that outlive any one implementation of them.
A bug report goes in `bugs/`, named for the symptom rather than the cause.

When a plan lands, it does not move — it gains a section saying what shipped
and how that differed from the proposal, and its row here changes. That is
cheaper than a migration of links, and the reasoning stays next to the decision
it produced.
