# Encryption

**Decision, 2026-08-18: Silo will not support the encrypted libraries it
inherited from upstream.** We built our own end-to-end scheme instead. The
audit that forced that decision is kept
[at the end of this document](#why-not-the-inherited-scheme); it is history
now, and reading it is optional.

**This document is no longer the design.** It began as the sketch that
established a modern scheme was reachable. The design has since been written
for real: the wire format — chunking, content crypto, manifests, names — is
specified in [`spec/store-format.md`](spec/store-format.md), and the account
model that holds the key material is pinned in [`auth.md`](auth.md). Where this
document and those disagree, they bind. What stays alive here is the
orientation below, the guardrails, and the record of what the sketch got
superseded on.

## The scheme

Two tiers of key, and the indirection between them is what buys everything
else:

- **An identity keypair per account** (X25519), generated client-side at
  enrolment. The public key is published — it is what another member's client
  wraps a library key *to*, so it must be servable to people who are not its
  owner. The private key is sealed under a key derived client-side from the
  passphrase and stored server-side as an opaque blob
  (`AccountIdentityKey.wrapped_key` in [`auth.md`](auth.md)), so a new device
  can bootstrap from the passphrase alone. The server can store that blob but
  never open it: login uses a *split derivation* — the client stretches the
  passphrase into an `authKey` and only that crosses the wire, while the
  sealing key lives on the branch of the derivation the server is never sent.
- **A content key per library** (CK), 32 random bytes generated client-side at
  creation, never derived from a password. Everything in the library derives
  from CK: the chunk keys, the name encryption, the chunker seed. CK is
  **wrapped to each member's public key** — one sealed blob per member, bound
  to the recipient so blobs cannot be swapped or replayed across libraries.
- **Ten recovery codes per account**, each stored as its own independent wrap
  of the identity key (`AccountRecoveryWrap`, one row per code). Redeeming one
  deletes its row and leaves the other nine valid — the server never learns a
  code; redemption is the client fetching the set and trying each blob.

The inherited scheme bound the library to a password, so sharing meant telling
someone the password and revocation was impossible. Wrapping to identities means sharing is
"wrap CK for one more public key", and a passphrase change re-wraps only the
user's *own private key* — CK is untouched and not one byte of content is
re-encrypted. Recovery protects exactly one secret, the identity key, and
through it every library the account can open.

### Content

Chunking is **keyed FastCDC** — the same `fastcdc-gear64/v1` as a plain
library, but seeded from CK (`HKDF-SHA256(CK, salt="silo/chunker/v1")`), so cut
points are a function of plaintext *and a secret*. The sketch that used to live
at the top of this file mandated fixed-offset chunks on the grounds that CDC
cut points leak plaintext structure; the spec closed that leak by keying the
cut points instead, which keeps delta sync and gives the server nothing to
fingerprint. **An E2EE library must never chunk under the plain seed** — see
the Seeds section of the spec for the second, subtler reason (`seal_hash`
would otherwise be a content-confirmation oracle).

Each chunk is sealed under a key derived from its own plaintext
(spec, [Content crypto](spec/store-format.md)):

```
H_p   = SHA-256(plaintext_chunk)
K_c   = HKDF-SHA256(CK, salt="silo/chunk/v1", info=H_p, L=32)
frame = AES-256-GCM(K_c, zero nonce, plaintext_chunk)
id    = SHA-256(frame)
```

Convergent on purpose: identical plaintext under the same CK yields an
identical frame and a stable id, so within-library dedup and delta sync work
exactly as in a plain library. What that costs is a content-confirmation
oracle extending only to holders of CK — the library's own members — and the
spec accepts that trade explicitly. The AEAD tag gives integrity per chunk and
localises tampering to the chunk it touched.

Ranged reads fall out for free, and this is worth being explicit about: the
server is not decrypting anything, so a range request is a plain byte range
over stored bytes. Manifests carry a mandatory per-chunk plaintext size, so
the client maps a plaintext range onto chunk indices, asks for the ciphertext
covering them, decrypts, and trims. The inherited scheme could not serve ranges
on encrypted libraries; this one serves them the same way it serves everything
else.

### Names and metadata

Settled — this used to be the open question that decided the shape of the API.
Each path segment is encrypted with **AES-CMAC-SIV, under a key derived per
directory** (spec, [Names](spec/store-format.md)). SIV is deterministic by
design, which is the point: `entries/{path}` routes on ciphertext, so a client
must be able to compute the same bytes the directory object holds. The
equality leak that determinism implies — identical names within a directory
encrypt identically — is stated and accepted. Directory and commit objects
additionally carry **sealed sections** for the metadata that has no reason to
be public, with sizes and structure bounded so the server can validate what it
stores without reading it.

### What it costs

- **Cross-library and cross-user dedup end for E2EE libraries.** A different
  CK gives a different seed, different keys, different frames. Decided and
  priced in the spec: "that was always the price of E2EE." Within one library,
  dedup survives in full.
- **No server-side anything**: no thumbnails, no preview, no full-text search,
  no server-side zip, no charset guessing. That machinery is gone anyway.
- **Compression must happen client-side, before encryption.** Ciphertext does
  not compress.
- **Revocation is not retroactive.** Rotating CK and re-wrapping for the
  remaining members stops future reads; a departing member keeps whatever
  ciphertext and old CK they already had. Only re-encrypting the library fixes
  that, and that is O(library). Said plainly rather than implied otherwise.

## Status

The format is specified with test vectors. The server side already treats an
E2EE library as opaque in the ways that matter — the store hashes what it
stores, and the changes feed works on a library the server cannot read, which
is the point of the design.

**The server half is built.** The account key schema landed — the four items in
[`auth.md`](auth.md#the-accounts-key-material): `client_kdf_params`,
`AccountIdentityKey`, `AccountRecoveryWrap`, and the pre-login parameters
endpoint, `POST auth/kdf`. That endpoint was the sharp piece and its
constraints remain load-bearing rather than historical: it is unauthenticated
by necessity, answers unknown addresses with stable plausible parameters so it
cannot be used to ask which addresses exist, and its answer is
attacker-influenced input to the client's KDF — which is what the parameter
ceiling in the store package exists to bound. Creating an encrypted library
landed with it: `POST /libraries` with `"e2ee": true`, and
`GET /libraries/{id}/key`, behind the `account-keys` and `e2ee-libraries`
feature names. See [`storage.md`](storage.md) § What the server can read for
the request shape and why the library id comes from the client.

What is left is **a client that does the sealing**, and one server-side
sequencing item: the split-derivation login, where the password is stretched
under the parameters `auth/kdf` serves and only the auth half goes up. Both are
tracked in [`roadmap.md`](roadmap.md).

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
`entries/` path. `parseContentType` (`entries.go:876`) guessing a MIME type
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

**Per-user key material has a designed home — build that, not a variant.**
This guardrail used to say "leave room"; the room is now drawn, in `auth.md`'s
schema. The failure mode has moved: improvising a slightly different shape
because the designed one is not implemented yet.

**Keep client-supplied library ids possible.** Key wrapping binds to the
library id, so the client has to be able to create a library with a UUID it
chose. The inherited scheme's `magic` value had the same constraint for worse
reasons; the constraint outlived `magic`.

**Do not ship features that require plaintext.** Full-text search, thumbnails,
office preview, virus scanning, server-side folder zip. Each is defensible on
its own and each becomes a reason not to do this. If one is genuinely wanted,
scope it explicitly to unencrypted libraries so the boundary is visible in the
code rather than discovered later.

## What the sketch got superseded on

The original sketch in this file established reachability; the spec then made
different calls on four of its specifics. Recorded so nobody resurrects the
sketch's version from history:

- **Fixed 1 MiB chunks → keyed FastCDC.** The sketch ruled out CDC because cut
  points leak structure; the spec keys the cut points with a CK-derived seed
  instead, keeping delta sync and closing the leak.
- **XChaCha20-Poly1305 with random per-file nonces → convergent AES-256-GCM
  per chunk.** The sketch derived a file key from a random nonce; the spec
  derives a chunk key from the chunk's own plaintext hash, which is what makes
  within-library dedup survive encryption.
- **The dedup question → decided.** The sketch left "random nonces vs
  convergent" open; the spec chose convergent within a library and accepted
  the members-only confirmation oracle explicitly.
- **Names Option A vs B → settled as A, hardened.** Deterministic AES-SIV per
  segment so path routing keeps working, plus sealed sections in directory and
  commit objects for what need not be public.
- **"Delete `keycache/` and `parseCryptKey`" → done**, along with the entire
  lane they were wired into.

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

This file previously also held a plan for *creating* libraries in that format
from the TUI. That plan is withdrawn, and it was wrong on a load-bearing
detail: it claimed `enc_version=4` was AES-128-ECB and "crypto-identical to
v3". v4 is AES-256-CBC. Implementing it as written would have produced
libraries no client of that server could open. Do not resurrect it from git
history.

## Why we could walk away

For as long as the question was open, Silo could not create an encrypted
library at all — the old `CreateLibrary` wrote `is_encrypted=0` and no endpoint
accepted a password. Upstream only ever created them from browser JavaScript in
a web layer Silo does not run.

So there was no installed base. No Silo user had an encrypted library that Silo
made, and the only way to have one at all was to import it from an upstream
install. Refusing the format cost us nothing we offered, and bought a free hand
— which is what `POST /libraries` with `"e2ee": true` now spends.

As for any library in the old format that might still sit in a data directory:
the sync lane that could read one was deleted whole (`5d4baa0`), and the
server-side decryption stubs (`keycache/`, `parseCryptKey`) with it. Such a
library is unreadable through Silo today, and the listing's `encrypted` flag is
how a client knows to say so rather than discovering it per file.
