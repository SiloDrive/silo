# Error reporting

Silo's server can report its own failures to any Sentry-compatible receiver —
[Splat](https://github.com/dkam/splat), GlitchTip, Bugsink, or sentry.io
itself. It is off until you give it a DSN, and off means off: no client is
constructed, no logging hook is installed, and no middleware is added to the
request chain.

```sh
SILO_SENTRY_DSN=https://<public-key>@splat.example.com/1 silo serve
```

`SENTRY_DSN` works too, so a deployment that already sets one for its other
services does not need a Silo-specific variable.

## Checking that it works

```sh
silo sentry-test
```

Sends one error and one transaction through the same client the server uses,
and says what the receiver made of them:

```
Reporting to http://splat.example.com:3304/silo
  environment production, release silo@v0.3.35

  envelope     accepted (HTTP 200)
  envelope     accepted (HTTP 200)

Delivered. Look for the issue "Silo test event — reporting is configured
correctly" and the transaction "GET /silo-sentry-test"; both are safe to delete.
```

This exists because the honest answer to "why is nothing showing up" is usually
"nothing has gone wrong yet" — the server reports failures, and a healthy
server produces none. That is indistinguishable from a DSN pointing at a closed
port, because the transport is asynchronous and silently drops what it cannot
deliver. `sentry-test` keeps the HTTP response the transport throws away, so
the two cases read differently:

```
  envelope     could not reach the server: dial tcp 10.0.0.5:3030: connect: connection refused
  envelope     rejected (HTTP 401): the key in the DSN is not the one this project expects
  envelope     rejected (HTTP 404): no project by that name — check the last path segment of the DSN
```

It exits non-zero on any failure, so it works in a health check. The one thing
it cannot tell you is whether the *receiver* stored what it accepted; a
Sentry-compatible endpoint answers 200 and ingests asynchronously.

## What gets sent

**Errors.** Silo reports failure by logging it, so a logrus hook is what makes
the roughly one hundred existing `log.Error` sites visible without touching any
of them. Everything at error level and above goes; warnings and below do not,
because a sync client asking for a library it no longer has access to logs a
warning several times a minute and would drown everything else.

**Panics.** Every `recover()` site in the tree reports as well as logs, and the
report is made from inside the deferred function while the panicking frames are
still on the stack — that is what makes the stack trace point at the code that
came apart rather than at the recovery. Handler panics are caught by the
outermost middleware, reported with the request attached, and then re-panicked
so `net/http` still tears the connection down exactly as it did before.

**Request timings.** A sampled share of requests is reported as a performance
transaction, which is what feeds the receiving end's latency percentiles and
endpoint rankings. Request *bodies* are never read: they are file contents, and
a crash report should not be a copy of somebody's document.

## Grouping

A receiver that has no fingerprint to go on groups events by hashing the
message, and almost every message Silo logs has a library id, a block hash, a
path or an account in it. Left alone that files one issue per file, which is
the same as having no issue list at all.

So Silo sends its own fingerprint: the function that logged, plus the message
with its variable parts replaced — ids, hashes, numbers, addresses and quoted
strings. `failed to read block a94a8f…` and `failed to read block da39a3…`
become one issue that has happened twice, which is the thing you actually want
to know.

Transactions also carry the response status at `contexts.response.status_code`,
which is where the protocol puts it and where a receiver reads it. The Go SDK
records the status only as span data under `contexts.trace`, so Silo sets the
response context itself — without it every transaction arrives with a blank
status and a page of 500s is indistinguishable from a page of 200s.

Transactions get the same treatment from the other direction. Named by URL,
`/repo/{repoid}/block/{id}` would arrive as hundreds of thousands of distinct
endpoints of one request each; a middleware renames each transaction after the
route mux matched, so they aggregate.

## Sampling

`SILO_SENTRY_TRACES_SAMPLE_RATE` defaults to `0.1`. A sync client polls every
few seconds and a single large upload is thousands of block PUTs, so tracing
everything sends a great deal of data that says the same thing repeatedly. Set
it to `1` while you are looking at something specific, and to `0` to turn
tracing off and keep only the errors.

Three endpoints are never traced, whatever the rate: `/notification`, because it
is a WebSocket held open for as long as the client runs and its "request" would
be a transaction lasting hours; `/debug/pprof/*`, which is already being watched
by whoever asked for it; and `/protocol-version`, which is a constant. Requests
answering 404 are dropped by the SDK's own default, which is what stops a port
scanner from inventing an endpoint per probe.

## What is not reported

The TUI and the CLI subcommands do not report. They run on someone else's
machine and fail in front of the person who ran them, so there is nobody for a
report to tell anything they cannot already see.

## Privacy

`SendDefaultPII` is on, so events carry the request URL, headers, client
address, and the account behind the request where the SDK can see it. That is
deliberate: the point of a report is to be able to say whose sync broke. Request
and response bodies are never captured. If you are pointing Silo at a receiver
shared with people who should not see that, don't — run your own; that is what
Splat is for.

The DSN's public key is never logged. The startup line names only the scheme,
host and project it is reporting to.

## Configuration

| Variable | Purpose | Default |
|---|---|---|
| `SILO_SENTRY_DSN` | Where to send reports (`SENTRY_DSN` also works) | — (send nothing) |
| `SILO_SENTRY_ENVIRONMENT` | Environment name on events (`SENTRY_ENVIRONMENT` also works) | `production` |
| `SILO_SENTRY_RELEASE` | Release name on events (`SENTRY_RELEASE` also works) | `silo@<version>` |
| `SILO_SENTRY_TRACES_SAMPLE_RATE` | Share of requests traced, `0` to `1` | `0.1` |
| `SILO_SENTRY_SERVER_NAME` | Distinguishes instances reporting to one project | hostname |
| `SILO_SENTRY_DEBUG` | Log what the SDK is doing, for when nothing arrives | `false` |

A DSN the SDK cannot parse disables reporting with a warning rather than
stopping the server. Monitoring that refuses to let the thing it monitors start
is a worse outage than anything it would have caught.
