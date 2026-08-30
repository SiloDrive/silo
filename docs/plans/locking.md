# Plan: file locking

**Status: not built, and parked.** Per-file advisory locks, so two clients
editing the same document do not clobber each other — a feature upstream had,
and one Porter would want for the same reason.

## Endpoints, on the Silo lane

- `PUT    /api/silo/v1/libraries/{id}/locks/{path}`
- `DELETE /api/silo/v1/libraries/{id}/locks/{path}`
- `GET    /api/silo/v1/libraries/{id}/locks`

## Behaviour

- The lock owner is the authenticated user; a lock blocks writes from any
  other user until released or expired.
- Two lock types upstream: **manual**, with no expiry and an explicit unlock,
  and **auto**, with a short TTL refreshed on each save. Start with manual and
  add auto later.
- The enforcement point is the write paths — the `entries` `PUT` and the
  batch and commit lane — which reject with `403` when the target path is
  locked by someone else.
- Surface lock state in directory listings (`is_locked`, `lock_owner`,
  `lock_time`) so a client can render a padlock without a second call.

## The open question is E2EE

A *path* is ciphertext to the server, so the lock key has to be whatever the
client can name — probably the encrypted segment path, which the server can
compare without reading. That is the piece to design before writing anything;
the rest is plumbing.

## It should not ship without the push event

Locks are the clearest case for pushing over the notification WebSocket rather
than relying on poll-then-list. When a lock is taken or released, publish a
`file-lock-changed` event to every subscriber of the library, so clients
repaint immediately instead of waiting for the next directory refresh.

Concretely: land the server-side publish hook in the same change as the lock
itself, or ship a half-real-time feature and then have the padlock be the thing
users learn not to trust.

## Why it is parked

Nothing multi-writer is deployed. The feature's whole value is two people
editing one document, and there is currently at most one person. It also wants
[`plans/sharing.md`](sharing.md)'s grant model first, because "blocks writes
from any other user" presumes more than one user can reach the library at all.

[`protocol-frontends.md`](../protocol-frontends.md) notes that a WebDAV
frontend's `LOCK` verb becomes real once this lands, which is the other
consumer worth knowing about.
