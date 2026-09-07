# Upgrading a client to 0.5.0

**record.** This document writes out the vocabulary 0.5.0 retired, because a
client author has to grep for it. Everything else says library, because that is
now the only word.

0.5.0 is the first release that breaks the wire. v1 had never been tagged, so
there is no shim and no alias — the old spellings answer **404**, immediately
and on every call.

## Routes

    /api/silo/v1/repos                     ->  /api/silo/v1/libraries
    /api/silo/v1/repos/{repoid}            ->  /api/silo/v1/libraries/{libraryid}

and the same substitution on every sub-path: `/changes`, `/batch`,
`/chunks/missing`, `/chunks/{id}`, `/objects/{id}`, `/head`, `/entries/{path}`.

`/notify-token` is not on that list because it is not renamed, it is **gone** —
under either spelling. See [the notification socket](#the-notification-socket)
below.

`/api/silo/v1/server-info`, `/api/silo/v1/auth/login` and the `/notification`
WebSocket are unscoped and did not move.

## JSON

    "repo_id"      ->  "library_id"     every payload that carries one
    "repos"        ->  "libraries"      the notification subscribe body's array

Inside that array, `id` is **unchanged** and `jwt_token` is gone; see below.
The chunk surface's bodies never named a library at all — `{"chunks":[…]}` out,
`{"missing":[…]}` back — so they are covered entirely by the path change.

## The notification socket

`POST …/notify-token` is removed, the `jwt_token` field in a subscribe entry is
ignored, and no `jwt-expired` frame is ever sent. A socket authorizes from the
credential its handshake carried:

1. Set `Authorization: Bearer <credential>` **on the upgrade request**. It was
   optional before and is now required — a `/notification` upgrade with no
   credential, or a bad one, is `401` before it becomes a socket.
2. Send `{"type":"subscribe","content":{"libraries":[{"id":"<library>"}]}}`
   with no `jwt_token`, or `{"type":"subscribe","content":{"account":true}}`
   for one subscription covering everything the account can see.
3. Handle `subscribe-denied` where you handled `jwt-expired`. It means
   re-check access, never re-mint — there is nothing to mint.

Delete the minting code rather than keeping a fallback: `notify-token` answers
`404` now, and a `404` there is indistinguishable from notifications being
switched off. Branch on `notifications-credential` in `server-info` instead,
which is exactly what it is for. Full detail in
[`protocol.md`](protocol.md#change-notifications--ws-notification), and the
reasoning in
[`plans/notifications-account.md`](plans/notifications-account.md) § Open.

## The notification frame type

    "repo-update"  ->  "library-update"

**This is the one that fails silently, and it is worth handling first.** A
client whose frame switch ignores types it does not recognise is correctly
written — polling still covers correctness, so an unknown type is latency
rather than an error. Which means a client that misses this rename does not
break. It stops receiving push, falls back to polling, and logs nothing above
debug. Every other item on this page answers 404 the first time you get it
wrong.

## Feature names

    "repo-rename"  ->  "library-rename"

and `"libraries"` is new. Check for it before the first authenticated request:
a server that does not advertise it predates this release, and the 404 that
follows on the library listing reaches a person as an empty account rather than
as two builds that disagree.

## What did not change

upstream's own names. It shipped a `…-Repo-Token` header and an `/api2/repos` lane,
and those keep their spelling wherever this project quotes them, because
renaming another product's header would document one that never existed. The
lanes themselves were deleted from the server before this release.
