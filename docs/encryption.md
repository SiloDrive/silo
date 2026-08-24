# Encryption

**Decision, 2026-08-18: Silo will not support Seafile's encrypted libraries.**

We will build our own end-to-end scheme instead. Nothing below is implemented.
This document exists to record why we walked away, to sketch the replacement,
and — the part that matters day to day — to list the things we must not build,
because each of them would quietly make the replacement impossible.

This file previously held a plan for *creating* Seafile-format encrypted
libraries from the TUI. That plan is withdrawn. It was also wrong on a load-
bearing detail: it claimed `enc_version=4` was AES-128-ECB and "crypto-identical
to v3". v4 is AES-256-CBC. Implementing it as written would have produced
libraries no Seafile client could open. Do not resurrect it from git history.

## Why not Seafile's scheme

Audited against `../seafile/seafile-server/common/seafile-crypt.c`, not from
memory or documentation.

| ver | cipher | KDF | salt |
|---|---|---|---|
| 1 | AES-128-CBC | `EVP_BytesToKey`/SHA1, 2¹⁹ iters | hardcoded, 8 bytes |
| 2 | AES-256-CBC | PBKDF2-HMAC-SHA256, **1000** iters | hardcoded, 8 bytes |
| 3 | AES-128-**ECB** | PBKDF2-HMAC-SHA256, 1000 iters | per-library, 32 bytes |
| 4 | AES-256-CBC | PBKDF2-HMAC-SHA256, 1000 iters | per-library, 32 bytes |

The hardcoded salt for v1 and v2 is eight bytes shared by every Seafile
installation in existence. The comment directly above its declaration reads
`/* Should generate random salt for each library. */`.

Four problems, worst first. Any one is arguable; together they are a scheme
from a different decade.

**1000 PBKDF2 iterations.** OWASP's figure for PBKDF2-HMAC-SHA256 is 600,000.
Silo's own account passwords already use `authmgr.PBKDF2Iterations = 600000`
with rehash-on-login, so the same binary would hash a login password 600× harder
than the password protecting a user's encrypted files.

**`magic` is a published offline-cracking oracle.** It is
`PBKDF2(library_id + password, salt, 1000)`, stored in plaintext in the commit JSON
and served to any authenticated client by `SeaDriveDownloadInfoHandler`. Anyone
with a database copy — or any account that can call download-info — gets an
offline verifier at a work factor a single GPU chews through at millions of
guesses per second.

**One key and one IV for the entire library, forever.** After unwrapping
`random_key`, both the file key and the IV come from `seafile_derive_key` over
that same value and the library salt. They never vary. Every block is AES-256-CBC
under an identical (key, IV) pair, which makes the encryption deterministic:
identical blocks produce identical ciphertext, and two files sharing a prefix
share a ciphertext prefix up to the byte they diverge. That is most of ECB's
leakage, from the mode chosen to avoid it.

**No authentication.** No MAC, no AEAD. Ciphertext is malleable and a hostile
server can flip bits that the client will decrypt without complaint. For a
scheme whose whole premise is not trusting the server, this is the one that
matters most in principle.

Metadata is not protected at all. `fsmgr` imports `crypto/sha1` and nothing
else: filenames, directory structure, file sizes and block boundaries are all
plaintext. Only content is encrypted.

Upstream knows. `pwd_hash` / `pwd_hash_algo` / `pwd_hash_params` in the commit
format are their replacement for `magic`, backed by argon2id in
`common/password-hash.c`. It is a serious fix to one of the four. Silo carries
those fields through `CommitToLibrary` / `LibraryToCommit` but computes and verifies
none of them.

## Why we can walk away

Silo has never been able to create an encrypted library — `libmgr.CreateLibrary`
hardcodes `is_encrypted=0` and no endpoint accepts a password. Upstream only
ever created them through Seahub's browser JavaScript, and we have no Seahub.

So there is no installed base. No Silo user has an encrypted library that Silo
made, and the only way to have one at all is to import it from an upstream
Seafile install. Refusing the format costs us nothing we currently offer, and
buys a free hand.

## What we would build instead

A sketch, not a specification. Everything here is open to revision until
someone writes the code — the point is to establish that a modern scheme is
reachable, and what it needs from the rest of Silo.

### Keys

- **Identity keypair per user**, X25519. The private key is wrapped under the
  user's passphrase with argon2id and stored server-side as an opaque blob, so a
  new device can bootstrap from the passphrase alone. The public key is
  published.
- **Content key per library** (CK), 32 random bytes generated client-side at
  creation, never derived from a password.
- CK is **wrapped to each member's public key** — one sealed blob per member.

The indirection is what buys everything else. Seafile binds the library to a
password, so sharing means telling someone the password and revocation is
impossible. Wrapping to identities means sharing is "wrap CK for one more
public key", and a password change re-wraps only the user's *own private key* —
CK is untouched and not one byte of content is re-encrypted.

### Content

- Fixed **1 MiB chunks**, not content-defined boundaries: CDC cut points are a
  function of the plaintext and leak its structure.
- Per-file key `FK = HKDF-SHA256(CK, file_nonce)`, where `file_nonce` is 16
  random bytes kept in the file's metadata.
- Chunk *i* sealed with **XChaCha20-Poly1305**, nonce `file_nonce || i`,
  additional data binding the file id, chunk index and chunk count so chunks
  cannot be reordered, duplicated, or moved between files.
- The AEAD tag gives integrity per chunk, and localises tampering to the chunk
  it touched rather than the whole file.

Ranged reads fall out for free, and this is worth being explicit about: the
server is not decrypting anything, so a range request is a plain byte range over
stored bytes. The client maps a plaintext range onto chunk indices, asks for the
ciphertext range covering them, decrypts, and trims. **The new scheme should
serve ranges on encrypted libraries, where Seafile's cannot.**

### Names and metadata — the open question

This one decides the shape of the API, so it should be settled before code.

**Option A — deterministic name encryption.** Each path segment encrypted with
AES-SIV under `HKDF(CK, "names")`, encoded base64url. `entries/{path}` keeps
working exactly as built; the server routes on ciphertext without knowing it.
Leaks name equality within a library, approximate name length, directory shape,
and file sizes.

**Option B — encrypted directory objects.** The server stores fs objects as
opaque blobs and the client walks the tree by id. Leaks the shape of the tree
and nothing else. Costs a second access pattern: `entries/{path}` is
path-addressed and simply does not apply, so encrypted libraries would need an
id-addressed surface alongside it.

A is cheap and compatible with everything we have just built. B is the one that
actually delivers "the server knows nothing". Recommendation is A first with B
kept reachable, which is mostly a matter of not hard-wiring path-addressing into
places that could take an id.

### What it costs

- **Cross-user dedup ends.** Random per-file nonces mean identical files
  encrypt differently for different libraries. Within one library, deriving
  `FK` from a plaintext hash instead of a random nonce (convergent encryption)
  restores dedup at the cost of a confirmation-of-file oracle to anyone holding
  CK — which is the library's own members, so it may be an acceptable trade.
  Decide deliberately.
- **No server-side anything**: no thumbnails, no preview, no full-text search,
  no server-side zip of a folder, no charset guessing. We are removing that
  machinery anyway.
- **Compression must happen client-side, before encryption.** Ciphertext does
  not compress. `docs/compression.md` describes a server-side story that cannot
  apply to encrypted libraries.
- **Revocation is not retroactive.** Rotating CK and re-wrapping for the
  remaining members stops future reads; a departing member keeps whatever
  ciphertext and old CK they already had. Only re-encrypting the library fixes
  that, and that is O(library). Say so plainly rather than implying otherwise.

## Guardrails — what not to build

The replacement is a long way off. The way it dies is not a decision to abandon
it; it is a series of small, individually reasonable features that each assume
the server can read content, until the assumption is load-bearing and removing
it means breaking things people use.

**Never add a server-side key cache or a `set-password` endpoint.**
`fileserver/keycache/` and `parseCryptKey` exist, are wired into both the
Seafile lane and `entries/`, and are dead: nothing in the tree calls
`keycache.SetKey`, so `parseCryptKey` can only ever return its 400. The obvious
tidy-up is to add the missing endpoint and make the feature work. Do not. That
endpoint's whole purpose is to hand the server a key, which is the assumption we
are deleting. Delete the cache instead.

**Object ids stay hashes of stored bytes, never plaintext.** `ETag: "v1-{id}"`
works over ciphertext precisely because the server hashes what it stores. If
anything ever computes an id from plaintext server-side, end-to-end is over that
same day.

**Names stay opaque to the server.** No case folding, no Unicode
normalisation, no behaviour derived from a file extension anywhere on the
`entries/` path. `parseContentType` guessing a MIME type from the suffix
(`fileop.go:88`) is exactly the pattern to keep out; the new lane already passes
`siloTextCharset = ""` rather than inheriting the Seafile lane's `charset=gbk`
guess, and that instinct is the right one.

**Mind the name budget.** `shouldIgnoreFile` rejects names that are not valid
UTF-8 or are 256 bytes or longer. Encrypted names must therefore be encoded, not
raw bytes, and base64url inflates by about 1.4× — which caps plaintext names
around 180 characters. Either accept that or raise the limit deliberately, but
know it is there.

**Range support must never depend on the server understanding content.**
`serveFile` currently refuses ranges on encrypted libraries because the *server*
is doing the decryption. Under the new scheme that branch should disappear
rather than grow. Anything that makes ranged reads smarter about file contents
takes us the wrong way.

**Leave room for per-user key material.** There is nowhere today to publish a
user's public key or store their wrapped private key. Whatever shape it takes,
do not design account management such that a user is permanently just a row with
a password hash.

**Keep client-supplied library ids possible.** Key wrapping may bind to the
library id, so the client has to be able to create a library with a UUID it
chose. Seafile's `magic` had the same constraint for worse reasons; the
constraint is worth preserving even though `magic` is not.

**Do not ship features that require plaintext.** Full-text search, thumbnails,
office preview, virus scanning, server-side folder zip. Each is defensible on
its own and each becomes a reason not to do this. If one is genuinely wanted,
scope it explicitly to unencrypted libraries so the boundary is visible in the
code rather than discovered later.

## What happens to Seafile encrypted libraries now

Today an encrypted library that arrived by import is readable only by sync
clients, which do their own crypto. Any read through `entries/` or the file
server hits `parseCryptKey` and gets:

```
400  Library is encrypted. Please provide password to view it.
```

That message is misleading — no endpoint accepts one, and none will. The honest
behaviour is to refuse explicitly, with a message that says the format is
unsupported, and to mark such libraries in the listing so a client can grey them
out rather than discovering it per file. Small change; not made yet.

## Before implementing

1. Settle names and metadata — Option A or B above. It decides whether
   `entries/{path}` covers encrypted libraries or they need their own surface.
2. Decide dedup: random per-file nonces, or convergent within a library.
3. Decide where key material lives on the wire, and what a user's published
   public key looks like as a resource.
4. Delete `fileserver/keycache/`, `parseCryptKey`, and the `IsEncrypted`
   branches in `serveFile` and `putEntryFile` — the server-side decryption path
   is dead code that currently reads as a feature.
