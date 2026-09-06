# Adding a number to a token response breaks clients that decode loosely

**Found:** 18 Aug 2026, while building the `notify-token` endpoint proposed in
`docs/feature-req/notify-token-on-the-silo-lane.md`.
**Status: the bug is silo-drive's, and is fixed.** Filed here because the trigger
was a Silo change, and because the same trap is waiting for the next field
added to any response an existing client already parses.

This is not a Silo defect. `expires_at` is right and it has shipped. This is
the note `protocol.md` § Writing a client points a client author at, so that the next one does
not walk into it.

## What happened

Every other token-issuing endpoint on the Silo lane returns a body whose values
are all strings:

```json
{"token": "…"}          POST /api/silo/v1/libraries/{id}/sync-token
{"jwt_token": "…"}      GET  /repo/{id}/jwt-token
```

So silo-drive decoded all of them through one helper:

```go
var result map[string]string
json.NewDecoder(resp.Body).Decode(&result)
```

which is correct, tested, and passed against every endpoint that existed at the
time it was written.

`notify-token` adds an expiry, because a client that knows when its 72-hour
token lapses can re-mint before the socket disconnects it rather than in
response to being disconnected:

```json
{"jwt_token": "…", "expires_at": 1787312025}
```

That body does not decode into a `map[string]string`. Go stops at the number:

```
json: cannot unmarshal number into Go value of type string
```

silo-drive would have failed on the endpoint it had itself requested, on the day
that endpoint shipped, having passed every test it had until then. Its own fake
server could not have caught it either, because the fake emitted only strings —
because every real endpoint did.

## Why it is worth writing down

The failure has an unpleasant shape. It is not caught by the client's tests,
because those describe the world before the change. It is not caught by the
server's tests, because the server is correct. It appears at the moment of
deployment, in a client the server author cannot see, and it presents as "the
new endpoint is broken".

And `map[string]string` is not a silly thing to have written. It is the
*obvious* thing to write against a family of endpoints that genuinely all
return string-to-string maps. The trap is that the shape was an accident of
what those endpoints happened to contain, and nothing in the contract said it
would stay that way.

## The general form

Adding a field to a JSON response is normally safe, and stays safe as long as
the field's *type* matches what is already there. The first field of a new type
in a previously uniform body is the one that breaks people — and it breaks them
at decode, before any of their logic runs, so a client that would happily have
ignored an unknown string cannot ignore an unknown number.

Nothing here argues against `expires_at`. It should exist; it is the difference
between a client re-minting on schedule and re-minting on failure. It is worth
knowing that it was a slightly bigger change than it looked, and worth a line
in the release note that carries it.

## What a client should do instead

Decode into a typed struct per endpoint, so an unknown field of any type is
skipped rather than fought:

```go
var result struct {
	JWT       string `json:"jwt_token"`
	ExpiresAt int64  `json:"expires_at"`
}
```

silo-drive's fake server now emits `expires_at` as a number specifically because it
is the field most likely to catch this class of mistake again. silo-drive's own
account of the bug is in `silo-drive-linux/docs/server-asks.md`.
