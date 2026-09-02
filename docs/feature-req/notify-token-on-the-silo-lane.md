# A Silo-lane way to mint a notification token

**Landed** in 0.4.4, as proposed: `fileserver/api/notify.go` and one route.
Silo Drive's fallback path can go. See [What was done](#what-was-done) at the end.

**Asked:** 18 Aug 2026, by Silo Drive, against 0.4.3.
**Size:** one handler and one route — 72 lines, prototyped and run end to end.
**Blocks:** a client that speaks only the Silo lane. Today there is exactly one
call that makes that impossible, and this is it.

```
POST /api/silo/v1/libraries/{libraryid}/notify-token
  → 200 {"jwt_token": "…", "expires_at": 1787312025}
```

## Why

Getting onto the notification socket currently takes two calls, and the second
one leaves the lane:

```
POST /api/silo/v1/libraries/{id}/sync-token   Authorization: Bearer …   → {"token":…}
GET  /repo/{id}/jwt-token                 <library-token header>: …  → {"jwt_token":…}
```

`protocol.md` § Writing a client is upfront that this is history: the notification endpoint
predates the Silo lane and authenticates the way the upstream client does.
Silo Drive implements it exactly as documented, and it works — verified against a
live 0.4.2, push landing in tens of milliseconds.

So this began as a tidying ask. It stopped being one when Silo Drive grew a
credential model.

Silo Drive now presents **no credential by default**, for a server that asks for
none. Every call it makes works that way for free, because they all go through
one authorizer and an anonymous authorizer stamps nothing. This one does not:
it needs a *library token*, and a library token exists because the upstream
client needs one. The only part of Silo Drive that cannot be made credential-agnostic is
the part that speaks the credential model `auth.md` proposes replacing.

That is the argument. It is not "two requests instead of one" — it is that the
compatibility surface is currently load-bearing for a client that has no
business touching it, and it will still be load-bearing after device
credentials land unless this exists.

## What it is

`fileserver/api/notify.go`, new. It is `CreateLibrarySyncTokenHandler` with
`GenNotifJWTToken` in place of `GenerateLibraryToken`, and an `EnableNotification`
check in front:

```go
// notifyTokenTTL matches what the legacy-lane endpoint issues, because it is
// the same token. A holder is expected to re-mint rather than keep one for the
// life of a process.
const notifyTokenTTL = 72 * time.Hour

type notifyTokenResponse struct {
	Token     string `json:"jwt_token"`
	ExpiresAt int64  `json:"expires_at"`
}

func CreateNotifyTokenHandler(w http.ResponseWriter, r *http.Request) {
	// Checked before permission, so a server without the notification endpoint
	// says so plainly rather than answering 403 about a library the caller can
	// read perfectly well.
	if !option.EnableNotification {
		http.Error(w, "Notification server is not enabled", http.StatusNotFound)
		return
	}

	user := middleware.GetUserEmail(r)
	libraryID := mux.Vars(r)["libraryid"]

	if share.CheckPerm(libraryID, user) == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	expires := time.Now().Add(notifyTokenTTL)
	token, err := utils.GenNotifJWTToken(libraryID, user, expires.Unix())
	if err != nil {
		log.Errorf("Failed to generate notification token for library %s: %v", libraryID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, notifyTokenResponse{Token: token, ExpiresAt: expires.Unix()})
}
```

and one line in `fileserver/server.go`, beside the sync-token route it
replaces:

```go
apiRouter.HandleFunc("/libraries/{libraryid}/notify-token", api.CreateNotifyTokenHandler).Methods("POST")
```

`api` already imports `option`, `share`, `middleware` and `mux`. It adds `time`
and `utils`, and there is no cycle — `utils` imports `option` and nothing else
of Silo's.

## What does not change

**The notification server.** `parseNotifToken` verifies a signature, an
audience and a library id in a process with no database, and has never known how
the holder was authenticated. The token this issues is the same token, because
it is the same `utils.GenNotifJWTToken` call. Decoded from the prototype:

```json
{"library_id": "bc0a62c4-…", "username": "test@test.com", "aud": ["silo:notif"], "exp": 1787312025}
```

**`getJWTTokenCB` and the `/repo/{id}/jwt-token` route.** The upstream client
still needs them. Nothing is removed by this; the Silo lane simply stops
depending on them. (Both went with that lane in `5d4baa0`, after this was
written.)

What *does* change is who may ask for a token: `share.CheckPerm` on the
authenticated user, instead of possession of a library token. That is strictly
better under `auth.md`'s model — `CheckPerm` is where a credential's permission
ceiling will intersect, so a read-only or single-library credential gets the
right answer here automatically, whereas a library token is an all-or-nothing
bearer credential with no expiry that only exists to be exchanged.

## `expires_at`

Not present on `getJWTTokenCB`, and worth adding here.

Without it a client learns its 72-hour token has lapsed when the socket says
`jwt-expired` — after the fact, mid-session, as an error to recover from. With
it, a long-lived client re-mints *before* it is disconnected. Silo Drive does not
need it yet, because it mints one per subscription and a token cannot go stale
in its hands, but a client that cached one would.

One warning, learned the hard way: adding a number to a token response is a
client-visible change. Silo Drive decoded every token body into a
`map[string]string` — which is correct for `{"token":…}` and for
`{"jwt_token":…}`, and fails the moment a body carries a number. Silo Drive would
have broken on the endpoint it had itself requested, on the day it shipped,
having passed every test until then. That is Silo Drive's bug and it is fixed, but
it is the shape of thing worth knowing before adding a field to a response
other clients already parse.

## Verified

Built against silo HEAD (`5cd182a`) in a throwaway worktree, then driven by
Silo Drive. Nothing of the working tree or the running server was disturbed: the
prototype ran on `:8092` against a copy of the data directory.

Handler behaviour:

| request | result |
|---|---|
| with a valid session | `200 {"jwt_token":…, "expires_at":…}` |
| with no `Authorization` header | `401` |
| for a library the user cannot see | `403 Permission denied` |
| with `EnableNotification` off | `404 Notification server is not enabled` |
| claims in the issued token | `library_id`, `username`, `aud: [silo:notif]`, `exp` — identical to `getJWTTokenCB` |

Then Silo Drive mounted through a logging proxy, so "no legacy-lane calls" could be
counted rather than asserted. Every request it made over a mount, a read, a
poll and a push:

```
5 GET  /api/silo/v1/libraries
4 POST /api/silo/v1/libraries/{id}/notify-token
2 POST /api/silo/v1/auth/login
2 GET  /api/silo/v1/libraries/{id}/entries/
1 GET  /api/silo/v1/libraries/{id}/changes?since=…
1 GET  /api/silo/v1/server-info
1 GET  /notification
```

No `/repo/…`, no `/sync-token`, no `/api2/…`. Push still landed in single-digit
milliseconds, and getting subscribed cost one request per library where the old
path cost two.

## What Silo Drive deletes when this lands

`notifyJWT`, `syncToken`, the sync-token cache and the `notifyLane` field —
about 70 lines, and with them the only place in Silo Drive that builds its own
`http.Request` in order to avoid the function that attaches credentials.

Silo Drive is already written for it: `NotifyToken` asks for `notify-token` first,
falls back to the legacy pair on a 404, and tags the log line with the lane it
used, so the compatibility path stays visible rather than becoming a habit. The
day the endpoint exists Silo Drive uses it with no change at all.

The patch is kept at `silo-drive-linux/docs/silo-notify-token.patch`.

## What was done

Taken as written. `fileserver/api/notify.go` is the handler from this document,
unchanged apart from a comment explaining why the `EnableNotification` check
comes before the permission check; the route sits beside `sync-token` in
`fileserver/server.go`. No cycle, and `api` needed only `time` and `utils`
added, as predicted.

`expires_at` ships as a number. The hazard this document raises about that is
now its own note, `adding-a-number-to-a-token-response-breaks-clients.md` in
`docs/bugs/fixed/`, and `protocol.md` § Push warns the next client author in the place
they will be reading when it matters.

**Tests** (`fileserver/api/notify_test.go`) cover the handler's four exits and,
more usefully, the token itself: `TestCreateNotifyTokenClaims` verifies the
minted token the way `parseNotifToken` does — pinned algorithm, required
`silo:notif` audience — and checks `expires_at` equals the `exp` claim. A client
that re-mints on the advertised deadline while the real expiry is earlier gets
disconnected mid-session, which is the failure this field exists to prevent.
Read-only shares can mint, deliberately: subscribing to a library you can
already read tells you nothing polling would not.

A stranger and an absent library both answer 403, matching
`CreateAccessTokenHandler`, so the endpoint cannot be used to probe for valid
library ids.

**Documented** in `docs/protocol.md` (the reference, and § Push under *Writing a client*), where the
two-call legacy recipe has been replaced by the one call and demoted to a
paragraph about older servers.
