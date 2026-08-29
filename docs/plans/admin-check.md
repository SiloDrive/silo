# Plan: Admin privilege check (`is_staff`)

**Status: not built.** Nothing in the tree reads `is_staff` outside the schema
and the account backfill. This is where we intend to go, not what is there.

**Written before the identity split.** [`auth.md`](../auth.md) moves the flag
onto `Account`, which is where the code below should read it from — the shape
of the plan survives, but `IsStaff(email)` becomes a field on the account the
request already carries, and the snippets below are the pre-split spelling.

## Context

Silo's management API at `/api/silo/v1/` treats every authenticated user as equal — there is no way to gate endpoints behind an admin role. This blocks upcoming work on user management, library sharing admin views, and "view-only" accounts (see [`roadmap.md`](../roadmap.md)).

The groundwork is already in place: the `EmailUser` table has an `is_staff` column, `Account` has one beside it, and the bootstrap admin is written with it set. What's missing is (a) a helper that reads `is_staff` for an authenticated user, and (b) a middleware that refuses non-admin requests. This plan adds both as a minimal, surgical change — no existing behaviour is altered.

**Scope decisions (confirmed with user):**
- `is_staff` only gates future admin endpoints. `CheckPerm` and `ListLibrariesHandler` are **not** changed — admins do not implicitly see or write all libraries.
- Admin status is looked up per-request via a DB query (not embedded in the JWT), so revoking admin takes effect immediately.

## Changes

### 1. `fileserver/authmgr/authmgr.go` — add `IsStaff` helper

Add a new exported function alongside `ValidateSessionToken` (after line 168). It queries `EmailUser.is_staff` for the given email using the existing `readDB` handle and `option.DBOpTimeout` pattern used throughout this file.

```go
// IsStaff reports whether the given user has the is_staff flag set.
// Returns (false, nil) if the user does not exist.
func IsStaff(email string) (bool, error) {
    ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
    defer cancel()

    var isStaff bool
    row := readDB.QueryRowContext(ctx, "SELECT is_staff FROM EmailUser WHERE email=?", email)
    if err := row.Scan(&isStaff); err != nil {
        if err == sql.ErrNoRows {
            return false, nil
        }
        return false, err
    }
    return isStaff, nil
}
```

Note: verify `database/sql` is already imported in this file; if not, add it.

### 2. `fileserver/middleware/auth.go` — add `RequireAdmin` middleware

Add after `GetUserEmail` (line 46). It depends on `RequireAuth` having already populated `UserEmailKey`, so it will always be chained *after* `RequireAuth`.

```go
// RequireAdmin is middleware that rejects requests unless the authenticated
// user has is_staff=1 in EmailUser. Must be chained after RequireAuth.
func RequireAdmin(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        email := GetUserEmail(r)
        if email == "" {
            http.Error(w, "Authentication required", http.StatusUnauthorized)
            return
        }
        isAdmin, err := authmgr.IsStaff(email)
        if err != nil {
            http.Error(w, "Failed to check admin status", http.StatusInternalServerError)
            return
        }
        if !isAdmin {
            http.Error(w, "Admin access required", http.StatusForbidden)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

No routes are wired up in this change. `RequireAdmin` is added as infrastructure for the next feature (user management API) to consume. This keeps the diff small and the behaviour of every existing endpoint unchanged.

## Critical files

- `fileserver/authmgr/authmgr.go` — add `IsStaff` after line 168
- `fileserver/middleware/auth.go` — add `RequireAdmin` after line 46

## Reused existing code

- `readDB` + `option.DBOpTimeout` pattern — same as `ValidatePassword` and `CreateAccount` in `fileserver/authmgr/authmgr.go`
- `middleware.GetUserEmail` + `UserEmailKey` context — `fileserver/middleware/auth.go:47`
- `Account.is_staff` column — already in `fileserver/dbutil/schema.go:43`, and now set by the setup token's claim (`setup.Claim` creates the first account as staff) rather than by a bootstrap admin. The old citation named `EmailUser`, a table that predates the library rename, and `authmgr.EnsureAdmin`, which no longer exists

## Verification

1. **Build**: `go build ./cmd/silo` — must compile cleanly.
2. **Lint/vet**: `go vet ./...`.
3. **Unit test** (add under `fileserver/authmgr/` if a test file exists there, otherwise a small new `authmgr_test.go`):
   - Create an `EmailUser` row with `is_staff=1` → `IsStaff` returns `true, nil`.
   - Create an `EmailUser` row with `is_staff=0` → `IsStaff` returns `false, nil`.
   - Unknown email → `IsStaff` returns `false, nil` (no error).
4. **Middleware smoke test** (can be deferred until a real admin route exists): temporarily wire a throwaway `/api/silo/v1/_admin_ping` handler behind `RequireAuth` + `RequireAdmin` in a scratch branch, then:
   - Hit it with the bootstrap admin token → `200`.
   - Create a second non-staff user, log in, hit it → `403`.
   - Hit it with no token → `401`.
   Remove the scratch handler before merging.
5. **Regression check**: exercise the existing `/api/silo/v1/libraries` endpoints with a non-admin user (create one by direct DB insert with `is_staff=0`) to confirm nothing has changed — the permission model for existing routes must be identical.

## Out of scope (explicitly)

- No changes to `share.CheckPerm` or `api.ListLibrariesHandler`.
- No new admin routes — this change only adds the helper + middleware.
- No JWT claim changes; tokens remain `{email, exp}`.
- No user management endpoints (that's the next feature, which will consume `RequireAdmin`).
