# Integration tests

Minitest against a **running** Silo server. They exercise the HTTP contract the
docs describe — status codes, headers, response shapes — which the Go tests
cannot: nothing in `fileserver/` stands up a database, so handler-level
behaviour is only ever reachable from outside.

## Running

Start a server on a throwaway data directory, then point the suite at it:

```bash
go build -o /tmp/silo ./cmd/silo
SILO_DATA_DIR=/tmp/silo-data SILO_PORT=8099 \
  SILO_ADMIN_EMAIL=admin@example.com SILO_ADMIN_PASSWORD=testpass123 \
  /tmp/silo serve &

cd test
SILO_URL=http://localhost:8099 SILO_EMAIL=admin@example.com SILO_PASSWORD=testpass123 rake
```

`SILO_PASSWORD` is required; the other two have defaults
(`http://localhost:8082`, `admin@example.com`).

Run one file with `ruby blocks_test.rb`, and one test with `-n
test_a_block_that_does_not_hash_to_its_id_is_refused`.

**Use a throwaway data directory.** The tests create libraries and delete them
in teardown, but a failure part-way leaves them behind, and nothing here is
careful about a directory you keep things in.

## What is covered

| file | surface |
|---|---|
| `auth_test.rb` | login, tokens, expiry |
| `libraries_test.rb` | create, list, delete |
| `tokens_test.rb` | sync and access tokens, and that a library you cannot see is indistinguishable from one that does not exist |
| `blocks_test.rb` | `blocks/missing`, `PUT blocks/{sha1}`, `?type=blocks`, hash refusal, resume |
| `batch_test.rb` | many operations as one commit, all-or-nothing, `If-Match` on the root |
| `pagination_test.rb` | `?limit`, the `Link` header, and the anchor withheld until the last page |

## Writing one

`silo_client.rb` is a thin wrapper over `Net::HTTP`, not an SDK — add the call
you need rather than reaching for a gem. Assert on the status code and the
header as well as the body: several of the contracts here *are* the status code
(`424` versus `400`) or the header (`Link`, `ETag`, `Accept-Ranges`), and a test
that only reads the body would pass while the contract broke.
