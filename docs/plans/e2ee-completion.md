# Plan: finishing E2EE — from a server that can store it to a person who can use it

Status: **proposed**. Owns the *sequence* only; every step below links to
the document that is normative for it, and where they disagree the owning
document wins, by [`../roadmap.md`](../roadmap.md)'s rule.

Tracked on git.booko.info: milestones `e2ee-client`, `split-login`,
`grants-and-invites`, `sharing-e2ee` in `Silo/silo`, and one issue per
bullet — numbers are given beside each step. Silo Drive's share lives in
`Silo/silo-drive-linux` and `Silo/silo-drive-macos`. The issues carry state; this
document carries the reasoning, and is not updated as they close.

## Where it stands

The server half is built and tested (`fileserver/encrypted_library_test.go`,
`store/`): the sealed format with vectors, account key material behind
`account/keys` and `auth/kdf`, encrypted-library creation, per-member content
key wraps in `LibraryKeyWrap (library_id, account_id)`, and an id-addressed
surface that works identically on plain and E2EE libraries. `changes`, history,
GC and census all read public sections only.

Three things stand between that and a person keeping files in an encrypted
library:

1. **No client seals anything.** Neither `client/` nor silo-drive calls
   `store.Seal*`, `UnwrapIdentity` or `UnwrapCK`. Every E2EE test builds its
   objects by hand. Nobody has done a write-spine-then-read round trip over
   HTTP from anything that is not the test.
2. **Login still sends the password.** Until `authKey` goes up instead, the
   server sees the secret that opens the identity blob at every login, which
   [`../storage.md`](../storage.md) § One password, split client-side names as
   the condition under which "E2EE is theatre". A client that ships E2EE
   before this ships a promise the server can break.
3. **One member per library.** `GET libraries/{id}/key` selects the caller's
   wrap and only the creator ever gets one. The table is already keyed for
   many; nothing writes a second row.

The order below is forced by two dependencies: 2 must land before any E2EE
client is *offered* to a person (not before it is built), and 3 waits on the
grant model in [`sharing.md`](sharing.md) step 1.

## Build order

### 1. A reference client that seals — `client/` in this repository

Issues silo#6–#11. The shared Go client is where the sealing lives, so silo-drive
and `cmd/silo` share one implementation rather than two that drift. It is
also the only way to get the round-trip test that does not exist yet.

What is there to build on: `store/` has every primitive (`SealChunk`,
`EncodeSealed`/`DecodeSealed*`, `WrapCK`/`UnwrapCK`,
`OpenIdentityWithPassword`, `ChunkerSeed`, the name cipher). `client/` is
plain-only today — `APIClient` with path-addressed methods, no id surface, no
key material. The *Keyring* named below does not exist; this plan invents it,
and it is the one new type the step needs: CK plus everything derived from it
(chunker seed, name keys), so no caller ever holds a bare CK.

- **Key bootstrap.** `client.OpenAccount(password)`: `POST auth/kdf` →
  `store.Credentials` → login → `GET account/keys` →
  `store.OpenIdentityWithPassword`. Returns an `*Identity` and forgets the
  password. `client.OpenLibrary(id)`: `GET libraries/{id}/key` →
  `store.UnwrapCK` → a `*store.Keyring` (CK, chunker seed, name keys).
- **Create.** `client.CreateEncryptedLibrary(name)`: mint CK and library id,
  seal the empty root and first commit, `WrapCK` to the account's own public
  key, `POST /libraries` with `"e2ee": true`. The test seed in
  `encrypted_library_test.go` is this function in prototype; move it.
- **Read.** Resolve a path by walking directory objects and encrypting each
  segment under its parent's salt ([`../protocol.md`](../protocol.md#the-e2ee-client-shape)
  § Reading an E2EE library); read a manifest; fetch chunks by
  `chunks/fetch`; decrypt per chunk. A plaintext `ReadAt(off, n)` is
  arithmetic over the manifest — this is where "ranged reads on E2EE" come
  from, and it is client code.
- **Write.** The spine rewrite: chunk under `ChunkerSeed(CK)`, seal chunks,
  `chunks/missing` → `POST chunks`, seal the manifest, then each directory up
  to the root carrying its salt forward and stamping only the changed
  directory's mtime, then the commit, then `PUT head` with `If-Match` and the
  re-read-rebuild-retry loop on `412`. The three rules under *A write rewrites
  the spine* in the brief are the tests.
- **One interface, two implementations.** The brief already asks for it:
  *list*, *read*, *write* answered by a plain implementation over
  `entries/{path}` and an E2EE one over the id surface, chosen once per
  library rather than at each call site.

**Tests:** a full round trip against a real `fileserver` — create, write a
tree, read it back, `changes?since=` reports it, a second client with the same
key reads it, a client with no key gets ciphertext names and `400` on content.
Plus the spine rules as unit tests over the objects a write produces. The
round trip has a home already: `fileserver/*_test.go` (package `silod`)
imports `client` today (`chunk_lane_test.go`, `notif_wire_test.go`), so it
needs no new harness and creates no import cycle.

**Order within the pass:** build 1 before 2. The plan's own gate is that
nothing built here is *offered* before 2 lands; building it first is what
gives 2 a consumer to test against, and `OpenAccount` is the one function
both steps touch. Landing 2 first would mean writing the login twice.

**Where it lands:** `docs/roadmap.md` § What is built gets a row;
`protocol.md` § The E2EE client shape stops describing and starts pointing.

### 2. Split-derivation login

Issues silo#12, #13. Owned by [`../storage.md`](../storage.md) § One password, split client-side
and its item 1; [`../auth.md`](../auth.md) item 2.

- **Server, small:** the switch-over write — new `AccountPassword.hash` (of
  `authKey`) and new `client_kdf_params` in one transaction. Gate the fast
  hash on the row carrying `client_kdf_params`; a row without them still
  takes a password. A `server-info` feature name (`split-login`) so a client
  knows which to send.
- **Client:** `client.OpenAccount` from step 1 derives `authKey` and sends
  that. Enrolment (`POST auth/setup`, password change) does the same and
  writes the parameters. The identity key, not `wrapKey`, goes in the
  platform keystore, for the reason storage.md gives.
- **Migration:** an existing account crosses over on its next password change
  or on an explicit re-enrolment; no server-side batch can do it because the
  server does not have `master`.

**Tests:** the server refuses a raw password on an account that has crossed
over; a crossed-over account's `client_kdf_params` and hash never disagree
under a crashed write; `curl -u` against such an account is `401`.

**Gate:** no E2EE client is offered to a person before this lands. Step 1 can
be built and tested before it; it just cannot be *shipped* before it.

### 3. Sharing an encrypted library

Issues silo#14–#16. **Blocked**: the grant model is not built. Owned by [`sharing.md`](sharing.md) step 1 (grants, roles, invites) and the
`WrapCK` comment in `store/wrap.go`: "sharing a library is one more call to
this".

- **Grant model first**, per sharing.md — this step does not begin until
  `CheckPerm` unifies over the grant table.
- **The wrap row is written by the sharer, not the server.** `PUT
  libraries/{id}/key/{account}` with a blob the sharer produced by `WrapCK`
  against the recipient's published public key (`GET account/keys` is
  already readable to others for exactly this). The server checks the sharer
  holds a wrap and a grant with share permission, stores the row, emits
  `share.created`. `GET libraries/{id}/key` needs no change: it already
  selects by caller.
- **A recipient with no published key cannot be shared to.** This is what
  makes invite redemption E2EE bootstrap: an inactive account minted by
  address has no public key, so a share to it must wait until redemption.
  Decide here whether to queue the share (row without wrap, wrap written on
  first login by the sharer's client) or refuse it; the plan prefers refuse
  with a clear `409`, because a queued wrap is a client obligation with no
  owner.
- **Revocation is re-keying.** Removing a member deletes their wrap row and
  their grant, which stops the server serving them; it cannot un-know CK. A
  member who cached CK reads any object they can still fetch, so revocation
  is complete only after `silo convert` re-encrypts under a fresh CK and a
  new history root ([`../storage.md`](../storage.md) § Conversion). Ship
  revoke with the re-key as a *client* operation and the doc saying so plainly
  — the honest version of this is the only version worth shipping.

**Tests:** a shared member reads the tree with their own identity; a revoked
member gets `404` on the key and `403` on the surface; a share to an account
with no key is `409`; a member cannot re-share beyond their own ceiling.

### 4. Silo Drive

Issues silo-drive-linux#1, #2 and silo-drive-macos#1, in those repositories. Owned by
silo-drive's own docs; listed here because it is where a person meets the result.
The macOS build cannot share `client/` — it is Swift, so E2EE there is a port of
`spec/store-format.md` validated against `store/testdata`, and is out of its
v1 scope.

- Refresh `silo-context/docs/` — its copy of the brief still says E2EE is
  unreadable, and `internal/vfs/write.go` and `backend.go` reason from it.
- Replace `internal/silo`'s plain-only read/write with the `client/`
  interface from step 1, so an encrypted library stops being a directory that
  answers nothing.
- Key handling: the identity key from the platform keystore, CK per library
  in memory for the mount's life, never on disk. The mount's config gains an
  enrolment step rather than a password field.

### 5. Tidy the surface

Lands with the steps above rather than after them; silo#22 (a status table)
is the one item here worth doing first, because it is what would have
answered "is at-rest encryption built?" without reading the code.

- `entries/{path}` on a keyless E2EE store answers `403` for everything today;
  routing on ciphertext names is designed and not built (silo#30). The client
  in step 1 reads structure by walking objects from the head commit, which
  works now; #30 is what lets a path-scoped credential read an E2EE library.
- Add `e2ee-read` to the feature list once step 1 has a consumer, so a client
  can tell a server that serves wraps from one that merely stores them.
- `docs/encryption.md` § Status moves "a client that does the sealing" to
  built; `roadmap.md` § critical path drops items 1 and 2 as they land.

## What this plan does not do

- **OIDC-only accounts.** They have no password to derive `wrapKey` from;
  auth.md calls that "a design, not a column". Out of scope here.
- **E2EE folder links and the share page.** sharing.md steps 4 and 5; they
  need per-file share manifests and a browser client, and sit after step 3.
- **Server-side key escrow of any kind.** The `keycache` guardrail in
  encryption.md stands: no endpoint ever accepts a password for a library.
