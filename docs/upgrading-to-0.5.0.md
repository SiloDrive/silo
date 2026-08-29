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
`/chunks/missing`, `/chunks/{id}`, `/objects/{id}`, `/head`, `/entries/{path}`,
`/notify-token`.

`/api/silo/v1/server-info`, `/api/silo/v1/auth/login` and the `/notification`
WebSocket are unscoped and did not move.

## JSON

    "repo_id"      ->  "library_id"     every payload that carries one
    "repos"        ->  "libraries"      the notification subscribe body's array

Inside that array, `id` and `jwt_token` are **unchanged**. The chunk surface's
bodies never named a library at all — `{"chunks":[…]}` out, `{"missing":[…]}`
back — so they are covered entirely by the path change.

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
