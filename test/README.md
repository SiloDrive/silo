# Integration tests

Minitest against a **running** Silo server — the built binary, over a real
socket, driven by a client that is not written in Go.

**The reason given here used to be that the Go tests could not stand up a
database.** That was true when this was written and stopped being true in
`dee67bd`: `fileserver/library_wire_test.go` stands a real server on a real
SQLite database with `httptest`, and most contract testing belongs there, where
it runs under `go test ./...` with no server to start. Keeping the old
justification nearly cost this suite its life, so here is the real one — two
things `wire()` cannot do:

- **It tests the router, not the binary.** `newHTTPRouter()` is called directly,
  so `main()`, `option.Load`, the `*.Init` wiring, the cleanup goroutines and
  the bootstrap-admin path are all skipped. A missing `credential.Init` in
  `RunUser` passed every Go test and would have panicked in production; it was
  caught by running the binary.
- **It cannot tell `null` from `[]`.** Go unmarshals both into the same nil
  slice. `chunks_test.rb` asserts `missing` is `[]`, with the comment *"null
  here breaks every client that is not Go"* — that assertion is unwritable in
  the language the server is written in.

So: put a contract test in Go by default, and put it here when it is about the
process, the wire, or a shape only a second language can see.

## Running

Start a server on a throwaway data directory, then point the suite at it:

```bash
go build -o /tmp/silo ./cmd/silo
SILO_DATA_DIR=/tmp/silo-data SILO_PORT=8099 /tmp/silo serve &

# The server no longer invents an account, so the suite needs one made for it.
# Not through the setup token: that is a one-shot the suite would have to parse
# out of a log. `user add` reads a piped password and is idempotent enough to
# put in a script.
echo testpass123 | /tmp/silo user -d /tmp/silo-data add admin@example.com

cd test
SILO_URL=http://localhost:8099 SILO_EMAIL=admin@example.com SILO_PASSWORD=testpass123 rake
```

`SILO_PASSWORD` is required; the other two have defaults
(`http://localhost:8082`, `admin@example.com`).

Run one file with `ruby chunks_test.rb`, and one test with `-n
test_a_chunk_that_does_not_hash_to_its_id_is_refused`.

**Use a throwaway data directory.** The tests create libraries and delete them
in teardown, but a failure part-way leaves them behind, and nothing here is
careful about a directory you keep things in.

## What is covered

| file | surface |
|---|---|
| `auth_test.rb` | login returning a session credential, refusals, protected routes |
| `libraries_test.rb` | create, list, delete |
| `chunks_test.rb` | `chunks/missing`, `PUT chunks/{sha256}`, `?type=chunks`, hash refusal, resume |
| `batch_test.rb` | many operations as one commit, all-or-nothing, `If-Match` on the root |
| `pagination_test.rb` | `?limit`, the `Link` header, and the anchor withheld until the last page |

## Writing one

`silo_client.rb` is a thin wrapper over `Net::HTTP`, not an SDK — add the call
you need rather than reaching for a gem. Assert on the status code and the
header as well as the body: several of the contracts here *are* the status code
(`424` versus `400`) or the header (`Link`, `ETag`, `Accept-Ranges`), and a test
that only reads the body would pass while the contract broke.
