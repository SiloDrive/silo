# Gate G2 — hash benchmark

Date: 2026-08-22
Status: **closed — SHA-256 confirmed.**

The measurement behind [`../storage.md`](../storage.md) § Three rules: which hash
names every chunk and object in the store. Two runs, one per architecture —
arm64 first, x86 appended — plus source inspection of each candidate library,
because one of them was doing something that would have quietly poisoned the
numbers.

  Environment

  go1.26.7 · GOARCH=arm64 · darwin · Apple M1 Max (8P+2E) · FEAT_SHA1, FEAT_SHA256, FEAT_SHA512 all present.

  Results

  Single-threaded (GOMAXPROCS=1), one-shot API, median of 5 × 1s. A reused-hasher variant (Reset/Write/Sum) matched within noise at every size, so per-call
  construction cost is not a differentiator for any candidate.

  ┌─────────────────────────┬────────┬─────────┬───────┬───────┬──────────────┬──────────────────┬──────────────────────────┐
  │         Library         │ 64 KiB │ 256 KiB │ 1 MiB │ 8 MiB │ ns/op @1 MiB │ allocs/op @1 MiB │     arm64 assembly?      │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ crypto/sha1             │ 2391   │ 2396    │ 2395  │ 2392  │ 437,881      │ 0                │ Yes — ARMv8 SHA1 ext     │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ crypto/sha256           │ 2338   │ 2362    │ 2366  │ 2353  │ 443,098      │ 0                │ Yes — ARMv8 SHA2 ext     │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ sha512.Sum512_256       │ 1397   │ 1399    │ 1401  │ 1399  │ 748,294      │ 0                │ Yes — ARMv8.2 SHA512 ext │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ x/crypto/blake2b        │ 756    │ 748     │ 751   │ 743   │ 1,396,986    │ 0                │ No — amd64 only          │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ zeebo/blake3            │ 662    │ 662     │ 662   │ 658   │ 1,585,011    │ 0                │ No — amd64 only          │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ lukechampine.com/blake3 │ 579    │ 602     │ 612   │ 617   │ 1,714,354    │ 171              │ No — amd64 only          │
  ├─────────────────────────┼────────┼─────────┼───────┼───────┼──────────────┼──────────────────┼──────────────────────────┤
  │ zeebo/xxh3 (ceiling)    │ 26277  │ 26186   │ 26245 │ 25465 │ 39,953       │ 0                │ Yes — NEON               │
  └─────────────────────────┴────────┴─────────┴───────┴───────┴──────────────┴──────────────────┴──────────────────────────┘

  Throughput in MB/s.

  How the assembly column was determined (build tags + source, not numbers):
  - stdlib — sha1block_arm64.s uses SHA1C/SHA1M/SHA1P/SHA1SU0; sha256block_arm64.s uses SHA256H/SHA256SU0; sha512block_arm64.s uses SHA512H/SHA512SU0. Each
    gated on a cpu.ARM64Has* flag. Confirmed live: GODEBUG=cpu.sha1=off,cpu.sha2=off,cpu.sha512=off collapses them to 411 / 305 / 447 MB/s — 5.8×, 7.7×, 3.1×
    drops.
  - blake2b — only blake2b_amd64.s and blake2bAVX2_amd64.s; blake2b_ref.go is //go:build !amd64, so arm64 takes blake2b_generic.go.
  - zeebo/blake3 — every .s is impl_amd64.s; hash_avx2/impl_other.go and compress_sse41/impl_other.go (//go:build !amd64) forward to hash_pure/compress_pure.
  - lukechampine/blake3 — compress_amd64.s only; compress_noasm.go is //go:build !amd64 → compressBufferGeneric. Its MaxSIMD = 16 constant is unconditional,
    but there is no SIMD behind it on arm64.
  - xxh3 — real accum_vector_neon_arm64.s, selected via hasNEON = true in accum_stubs_arm64.go.

  Requirement 3 flag — lukechampine.com/blake3 multithreads implicitly, and cannot be turned off. Hasher.Write spawns one goroutine per eigentree,
  and guts.CompressEigentree spawns numChunks/16 more, uncapped by NumCPU. Sum256 routes through Write for anything over 1
  KiB, so the one-shot API is affected too. Measured peak: +384 goroutines for a single 8 MiB hash, vs +0 for zeebo. At default GOMAXPROCS it posts 4001 MB/s
  at 8 MiB — that number is ~10 cores of work, and it drops to 617 MB/s when held to one. Both libraries agree with the official BLAKE3 abc vector and with
  each other at 8 MiB, so this is genuine parallelism, not corruption — but in your design, where chunks are already hashed concurrently across goroutines, it
  would oversubscribe rather than help.

  Recommendation

  Use crypto/sha256. On this machine it runs at 2353 MB/s — within 2% of your SHA-1 baseline, so the migration is essentially free on Apple Silicon — and 3.5×
  faster than the quickest BLAKE3 binding actually available to you here. BLAKE3's speed reputation is an AVX2/AVX-512 fact that does not currently transfer to
  Go on arm64: neither binding has NEON assembly, so both run portable Go and land below SHA-512/256. Meanwhile SHA-1's collision resistance is broken
  (chosen-prefix collisions are practical), and in a content-addressed store the hash is the integrity proof — a collision lets an attacker substitute content
  under an existing address — so staying on SHA-1 is the one option the requirements rule out. SHA-512/256 is the interesting hedge, not SHA-256's rival here:
  it loses 40% on M1 but is the only candidate whose relative standing improves on x86.

  On cross-architecture inversion: yes, this ranking will very likely invert, and you should not ship a decision on the arm64 numbers alone. Every library that
  falls back to portable Go here has hand-written amd64 assembly: blake2b gets AVX2, zeebo/blake3 gets SSE4.1+AVX2, lukechampine gets AVX2+AVX-512. Expect all
  three to move up sharply on x86. The decisive variable is whether your x86 Linux box has SHA-NI (grep sha_ni /proc/cpuinfo) — Go dispatches blockSHANI ahead
  of blockAVX2 for SHA-256. On Zen 2+ or Ice Lake+ SHA-256 stays around 2 GB/s and my recommendation holds on both machines; on an older Xeon (Skylake-SP,
  Cascade Lake) SHA-256 falls back to AVX2 and will likely lose to AVX-512 BLAKE3, at which point zeebo/blake3 becomes the better cross-platform choice — it's
  the one that's fast on x86 without the threading hazard. Note also that x86 has no SHA-512 instruction at all, only AVX2, yet SHA-512/256 often still beats
  AVX2 SHA-256 there on 64-bit words — so SHA-512/256 could plausibly outrank SHA-256 on a non-SHA-NI x86 host while ranking clearly below it here. The
  benchmark is self-contained; run it on the x86 target before committing, since the address format is not something you'll want to change later.

---

## G2 x86 run — 2026-08-22

AMD Ryzen 5 5600 (Zen 3), `sha_ni` present · go1.27.0 linux/amd64 · GOMAXPROCS=1,
one-shot API, median of 5. Bench source: recreated per the table above
(the store format's hash gate).

| Library | 64 KiB | 256 KiB | 1 MiB | 8 MiB |
|---|---|---|---|---|
| crypto/sha1 | 2333 | 2339 | 2336 | 2334 |
| crypto/sha256 | 2189 | 2189 | 2193 | 2187 |
| sha512.Sum512_256 | 932 | 929 | 932 | 928 |
| x/crypto/blake2b | 1119 | 1121 | 1120 | 1120 |
| zeebo/blake3 | 5038 | 5211 | 5174 | 5150 |
| lukechampine/blake3 | 1881 | 2399 | 2627 | 2778 |
| zeebo/xxh3 (ceiling) | 75127 | 74664 | 71563 | 69123 |

Throughput in MB/s.

The prediction above was half right: SHA-NI does hold SHA-256 at ~2.2 GB/s —
but zeebo/blake3's AVX2 assembly posts 5.15 GB/s, 2.4× faster, so BLAKE3
*wins* on this x86 rather than merely surviving. The cross-architecture
picture is therefore an inversion in both directions:

| | M1 Max (Go) | Zen 3 (Go) |
|---|---|---|
| crypto/sha256 | 2353 | 2193 |
| zeebo/blake3 | 662 | 5150 |

**Decision: SHA-256 stands**, per the store format's selection rule — nothing in the
system is hash-bound, so the pick is the hash that is uniformly
hardware-fast, dependency-free, and native in both Go (stdlib) and Swift
(CryptoKit) on every target. SHA-256 is within 8% of itself across both
machines; BLAKE3 in Go spans an 8× spread and would make the Mac-resident Go
clients (TUI, porter-fuse under Rosetta-less arm64) the slow outlier, fixable
only with cgo or per-platform C bindings. BLAKE3's 2.4× x86 win optimises
paths that are network- and disk-bound.

Caveat: this box is the author's workstation. If the deployment server
differs, rerun — the decision rule survives any SHA-NI-bearing x86 or ARMv8
crypto-extension host, and only a server *without* both would reopen it.
