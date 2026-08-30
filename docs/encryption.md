# Encryption

**Record.** This is the audit that ruled out the encrypted libraries Silo
inherited from upstream, and the guardrails that keep the replacement honest.
It is not the design: the wire format — chunking, content crypto, manifests,
names, key wrapping — is [`spec/store-format.md`](spec/store-format.md), which
is normative, and the account model that holds the key material is
[`auth.md`](auth.md). Where this document and those disagree, they bind.

**Decision, 2026-08-18: Silo does not support the inherited encrypted
libraries.** The audit that forced that decision is
[at the end of this document](#why-not-the-inherited-scheme).

## The scheme

Two tiers of key: an X25519 identity keypair per account, its private half
sealed under a client-derived key and stored server-side as an opaque blob;
and a random 32-byte content key (CK) per library, wrapped to each member's
public key. Sharing is one more wrap, a passphrase change re-wraps only the
identity key, and recovery codes protect exactly that one secret. Key
derivations, the wrap construction and the recovery-code rules are
[`spec/store-format.md`](spec/store-format.md) § Key wrapping; the schema is
[`auth.md`](auth.md) § The account's key material.

### Content

Keyed FastCDC seeded from CK, then per-chunk convergent AES-256-GCM under a
key derived from the chunk's own plaintext hash, with the id the SHA-256 of
the sealed frame. Within-library dedup and ranged reads survive; the
members-only content-confirmation oracle is the accepted price. Normative:
[`spec/store-format.md`](spec/store-format.md) § Chunking and § Content
crypto. **An E2EE library must never chunk under the plain seed** — the
spec's Seeds section says why `seal_hash` would otherwise be a key-free
confirmation oracle.

### Names and metadata

Each path segment is AES-CMAC-SIV under a per-directory key, deterministic so
`entries/{path}` routes on ciphertext; directory and commit objects carry
sealed sections for what need not be public. Normative:
[`spec/store-format.md`](spec/store-format.md) § Names.

### What it costs

- **Cross-library and cross-user dedup end for E2EE libraries.** A different
  CK gives a different seed, different keys, different frames. Within one
  library, dedup survives in full.
- **No server-side anything**: no thumbnails, no preview, no full-text search,
  no server-side zip, no charset guessing.
- **Compression must happen client-side, before encryption.** Ciphertext does
  not compress.
- **Revocation is not retroactive.** Rotating CK and re-wrapping for the
  remaining members stops future reads; a departing member keeps whatever
  ciphertext and old CK they already had. Only re-encrypting the library fixes
  that, and that is O(library).

## Status

The server half is built: the format with vectors, the account key schema and
`POST auth/kdf`, `POST /libraries` with `"e2ee": true` and
`GET /libraries/{id}/key`, behind the `account-keys` and `e2ee-libraries`
feature names.

The client half is built too. `client.LibraryFS` answers *list*, *read* and
*write* over either kind of library, chosen once by `Account.Open`; the
encrypted implementation opens an identity from a password, mints a library the
server cannot read, rewrites the spine on every write, and turns a
`changes?since=` path back into a plaintext one. The round trip runs against a
real server.

Login is split on both sides. `client.Enrol` publishes an identity key and
crosses the account over in one call, and `client.OpenAccount` logs in with the
derived `authKey`, so an account this client enrolled never sends the server
the secret that opens its identity blob. A password change carries the
identity key across rather than orphaning it. What remains is sharing and the
grant model. The sequence is
[`plans/e2ee-completion.md`](plans/e2ee-completion.md).

## Guardrails — what not to build

The way this dies is not a decision to abandon it; it is a series of small,
individually reasonable features that each assume the server can read content,
until the assumption is load-bearing and removing it means breaking things
people use.

**Never add a server-side key cache or a `set-password` endpoint.** The
inherited ones (`fileserver/keycache/`, `parseCryptKey`) are deleted. The
temptation will recur — some feature will want the server to "just briefly"
hold a key. That endpoint's whole purpose is to hand the server a key, which
is the assumption this design deletes. No.

**Object ids stay hashes of stored bytes, never plaintext.** ETags work over
ciphertext precisely because the server hashes what it stores. If anything
ever computes an id from plaintext server-side, end-to-end is over that same
day.

**Names stay opaque to the server.** No case folding, no Unicode
normalisation, no behaviour derived from a file extension anywhere on the
`entries/` path. `parseContentType` in `fileserver/entries.go` guessing a MIME type
from the suffix is exactly the pattern to keep away from that path.

**Mind the name budget.** `shouldIgnoreFile` rejects names that are not valid
UTF-8 or are 256 bytes or longer, and encrypted names must be encoded, not raw
bytes. The spec's Name rules section owns the exact arithmetic; the guardrail
is that whoever touches the limit does it deliberately, knowing ciphertext
inflation is why it binds earlier than it reads.

**Range support must never depend on the server understanding content.** The
old lane refused ranges on encrypted libraries because the *server* was doing
the decryption; that branch died with the lane. Anything that makes ranged
reads smarter about file contents takes us the wrong way.

**Per-user key material has one home — build against it, not a variant.**
The schema is `auth.md`'s. The failure mode is improvising a slightly
different shape because the piece that needs it is not implemented yet.

**Keep client-supplied library ids possible.** Key wrapping binds to the
library id, so the client has to be able to create a library with a UUID it
chose. The inherited scheme's `magic` value had the same constraint for worse
reasons; the constraint outlived `magic`.

**Do not ship features that require plaintext.** Full-text search, thumbnails,
office preview, virus scanning, server-side folder zip. Each is defensible on
its own and each becomes a reason not to do this. If one is genuinely wanted,
scope it explicitly to unencrypted libraries so the boundary is visible in the
code rather than discovered later.

## Why not the inherited scheme

Audited against upstream's C crypt implementation as it stood in August 2026,
not from memory or documentation.

| ver | cipher | KDF | salt |
|---|---|---|---|
| 1 | AES-128-CBC | `EVP_BytesToKey`/SHA1, 2¹⁹ iters | hardcoded, 8 bytes |
| 2 | AES-256-CBC | PBKDF2-HMAC-SHA256, **1000** iters | hardcoded, 8 bytes |
| 3 | AES-128-**ECB** | PBKDF2-HMAC-SHA256, 1000 iters | per-library, 32 bytes |
| 4 | AES-256-CBC | PBKDF2-HMAC-SHA256, 1000 iters | per-library, 32 bytes |

The hardcoded salt for v1 and v2 is eight bytes shared by every installation of
that server in existence. The comment directly above its declaration reads
`/* Should generate random salt for each library. */`.

Four problems, worst first. Any one is arguable; together they are a scheme
from a different decade.

**1000 PBKDF2 iterations.** OWASP's figure for PBKDF2-HMAC-SHA256 is 600,000.
Silo's own account passwords already use `authmgr.PBKDF2Iterations = 600000`
with rehash-on-login, so the same binary would hash a login password 600× harder
than the password protecting a user's encrypted files.

**`magic` is a published offline-cracking oracle.** It is
`PBKDF2(library_id + password, salt, 1000)`, stored in plaintext in the commit
JSON and served to any authenticated client. Anyone with a database copy — or
any account that could call download-info — got an offline verifier at a work
factor a single GPU chews through at millions of guesses per second.

**One key and one IV for the entire library, forever.** After unwrapping
`random_key`, both the file key and the IV come from one derivation over that
same value and the library salt. They never vary. Every block is AES-256-CBC
under an identical (key, IV) pair, which makes the encryption deterministic:
identical blocks produce identical ciphertext, and two files sharing a prefix
share a ciphertext prefix up to the byte they diverge. That is most of ECB's
leakage, from the mode chosen to avoid it.

**No authentication.** No MAC, no AEAD. Ciphertext is malleable and a hostile
server can flip bits that the client will decrypt without complaint. For a
scheme whose whole premise is not trusting the server, this is the one that
matters most in principle.

Metadata is not protected at all: filenames, directory structure, file sizes
and block boundaries are all plaintext. Only content is encrypted.

Upstream knows. A trio of `pwd_hash` fields in their commit format is their
replacement for `magic`, backed by argon2id. It is a serious fix to one of the
four. Silo carried those fields through and computed none of them.

One detail worth pinning because it is easy to get wrong: `enc_version=4` is
AES-256-CBC, not AES-128-ECB, and is not crypto-identical to v3. A writer
that treated them as the same would produce libraries no client of that
server could open.

## Why we could walk away

For as long as the question was open, Silo could not create an encrypted
library at all — the old `CreateLibrary` wrote `is_encrypted=0` and no endpoint
accepted a password. Upstream only ever created them from browser JavaScript in
a web layer Silo does not run.

So there was no installed base. No Silo user had an encrypted library that Silo
made, and the only way to have one at all was to import it from an upstream
install. Refusing the format cost us nothing we offered, and bought a free hand
— which is what `POST /libraries` with `"e2ee": true` spends.

As for any library in the old format that might still sit in a data directory:
the sync lane that could read one was deleted whole (`5d4baa0`), and the
server-side decryption stubs (`keycache/`, `parseCryptKey`) with it. Such a
library is unreadable through Silo today, and the listing's `encrypted` flag is
how a client knows to say so rather than discovering it per file.
