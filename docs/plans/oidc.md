# Plan: OIDC login through the device grant

**Status: proposed. Nothing is built.** The design is settled in
[`auth.md`](../auth.md) § OIDC and is not re-argued here; this is the order to
build it in, the decisions the design left open, and three places where
building it against the code turned up something the design did not say.

The shape, in one paragraph: Silo is the OAuth client and the RFC 8628
"device". A client asks Silo to start a login; Silo asks the IdP; the IdP's
code and URL come back through Silo for the client to display; the person
approves at the IdP on any browser; Silo verifies the ID token against the
IdP's JWKS, binds it to an account, and mints an ordinary `Credential`, which
the client collects by polling. The IdP is consulted once per enrolment and
never per request. The client never speaks to the IdP, holds no IdP secret,
and needs no OAuth library.

## Three things the design did not say

**1. `account.Create` is idempotent on the address, and that is a takeover if
OIDC provisioning calls it.** `CreateTx` looks the address up first and, when
it is taken, returns the *existing* account with `created == false`. That is
right for every caller today — a share, an invite and `silo user add` all mean
"the account this address belongs to". For OIDC provisioning it means an
untrusted issuer asserting `email: alice@example.com` is handed Alice's
account through the function that was supposed to create a new one. The
binding rules in auth.md say step 2 (match by address) happens only for a
trusted issuer; provisioning has to check `created` and refuse on `false`, or
step 3 performs step 2 with no trust check at all. A test must pin this before
the code exists.

**2. "A password row is the mark of arrival" stops being true.**
`invite.refuseEnrolled`, and the reasoning around `account.Tombstone`, tell a
placeholder account from a real one by whether it has a password. An
OIDC-only account has none — it is the reason `AccountPassword` is a separate
table — so the moment one exists, `refuseEnrolled` reads it as a tombstone:
an invite minted for an address whose person arrived through the IdP, and who
has since been deactivated, would redeem into their account. Arrival has to
become *a password row or an `AccountIdentity` row*, in one function that both
the invite package and the OIDC binding ask.

**3. PKCE is not part of the device grant — and Silo sends it anyway.**
auth.md said "client_secret_post + PKCE" for this flow. RFC 7636 protects an
authorization code on its way back through a redirect; RFC 8628 has no
redirect and no code, and the device code never leaves Silo. So it protects
nothing here. But Clinch, the IdP this is first tested against, requires a
challenge of every confidential client by default, device grant included, and
an IdP that does not want one ignores it. Sending it costs a SHA-256 and spares
every such operator a setting to find, so Silo sends one; auth.md says why.

## Decisions

### Who gets an account

This was the one question the design left open. Today an invite is the only
way a person gets an account. "Anyone the IdP will sign in" is a much wider
door, and the right width depends on the IdP. For your own Authentik, where
the IdP already decides who may use the application, it is the whole point.
For "Sign in with Google" it is anybody on earth.

What decides it is whether the IdP's claim "this person controls this address"
can be believed, because linking by a verified address and taking over an
account by asserting its address are **the same operation**. They differ only
in whether the assertion is true. For an IdP the operator runs, or Google
Workspace, which marks an address verified only on a domain the Workspace
owns, the link is the feature: a person who has signed in with a password can
sign in through the IdP instead and land in the same account. For an IdP
where somebody other than the address's owner can make the claim, the same
link hands the account to whoever made it. Examples are Entra in multi-tenant
mode, where any tenant's administrator can set any user's address (the 2023
"nOAuth" class), and an IdP that lets users edit their own address and keeps
it marked verified.

So there is one policy, `SILO_OIDC_ACCOUNTS`, and not two switches that
combine. The earlier draft had `TRUSTED` and `AUTO_CREATE`, and one of their
four combinations was a server nobody could get into.

| Policy | Linked identity | Verified address matches an account or an invite | Nothing matches |
|---|---|---|---|
| **`link`** (default) | that account | link it | refuse: ask for an invite |
| **`create`** | that account | link it | a new account |
| **`isolated`** | that account | refuse | a new account |

`link` is for the operator's own IdP, and invites stay the way in. `create`
is for an IdP that already decides who may use the application. `isolated` is
for an IdP whose address claims cannot be believed: every person arriving
through it gets an account of their own, and none of them is ever merged with
anything.

**`SILO_OIDC_ALLOWED_DOMAINS`** is checked before any of it. It is a comma
list, and an address outside it is refused whatever the policy says. It
matters most against a public issuer: Google is every Gmail user on earth, so
`create` against it with no domain list gives an account to anybody. Silo
refuses to start with `create` or `isolated` against a known public issuer
(`accounts.google.com`, `login.microsoftonline.com`) unless a domain list is
set. A public sign-up server is a thing somebody could want, and it is a thing
somebody should have to type.

The binding, run once per completed flow and in this order:

0. **The address is outside `SILO_OIDC_ALLOWED_DOMAINS`** (when set) → refuse.
1. **`(iss, sub)` is in `AccountIdentity`** → that account, in every policy.
   If the account is inactive, refuse. An administrator switched it off, and
   signing in at the IdP is not a way to switch it back on. From here on the
   address plays no part: someone who changes their address at the IdP to
   yours stays on their own account, because yours is linked to your
   subject.
2. **`email_verified` is true, the address is in `AccountEmail`, and the
   policy is `link` or `create`**:
   - an *arrived* account (see 2 above), active → link `(iss, sub)` to it,
     and log it at warning level, naming the account, issuer and subject. A
     merge the operator did not expect should be findable.
   - an arrived account, inactive → refuse, for step 1's reason.
   - a tombstone with an outstanding invite → spend the invite (its role,
     `redeemed_at`), activate, link. The IdP has done what the invite email
     was for: proven that this person controls this address.
   - a tombstone without an invite (a share to an address nobody enrolled) →
     claim it under `create`; under `link`, refuse as step 3 does.
   - under `isolated`, a matching address is refused outright: *"This address
     already belongs to a Silo account."* Neither linking nor creating is
     safe there.
3. **Nothing matched**:
   - under `create` or `isolated`, with a verified address, and
     `account.Create` reporting `created == true` → a new account with the
     default role, plus its `AccountIdentity`, in one transaction.
   - `created == false` → refuse. The address was taken between the lookup
     and the insert, and this refusal is what stops account creation from
     performing step 2 without step 2's policy check (see the first discovery
     above).
   - under `link` → refuse: *"There is no Silo account for alice@example.com.
     Ask an administrator for an invite."*

**An unverified address never identifies anyone and never provisions
anyone.** Provisioning under an unverified address would lock the address's
real owner out of it, because an address belongs to exactly one account. An
IdP that does not send `email_verified` at all, as Entra often does not, is
treated as sending false.

**`link` is only as good as the IdP's `email_verified`**, and not only as good
as who runs the IdP. An IdP that lets a user edit their own address without
re-proving it, or lets an administrator set one, makes that address a claim
and not a proof. Checking that per IdP is part of step 7.

**A server with no accounts cannot be claimed through the IdP.** While
`setup_required` is true, `POST auth/device` answers `409`. The first
administrator comes from the setup token as today. The alternative is the
first person to reach the IdP's device page becoming the administrator.

**An operator can always link by hand**, in any policy: `silo user identity
link <email> <issuer> <subject>` (step 6). It is the way to link an existing
account under `isolated`, and the repair when an IdP changes a person's
subject. Linking from a signed-in session — sign in with the password, then
run the device flow to prove the IdP identity too — is the self-service
version, and it is left out of this plan: it is a second mode on an endpoint
that should ship with one.

### The endpoints

Under `auth/`, beside `login`, `setup` and `redeem`, and on the same lane:
unauthenticated, because nothing is authenticated yet.

**`POST /api/silo/v1/auth/device`** takes the enrolment half of a login
request unchanged: `kind`, `client_name` (required), `client_id`, `perm`,
`scope`. It uses the same `enrolmentOpts`, so the two paths cannot disagree
about what a request may ask for.

```json
200 { "user_code": "WDJB-MJHT",
      "verification_uri": "https://id.example.com/device",
      "verification_uri_complete": "https://id.example.com/device?code=WDJB-MJHT",
      "poll_token": "…", "interval": 5, "expires_in": 600 }
```

`404` when OIDC is not configured, which matches the `oidc` feature being
absent. `409` while setup is required. `503` when the IdP cannot be reached,
which is the IdP's outage and not Silo's.

**`POST /api/silo/v1/auth/device/poll`** `{ "poll_token": "…" }` answers with
status codes and not with RFC 8628's error strings. Silo's other endpoints
already answer this way, and `429` with `Retry-After` is how Silo already says
"slow down":

| Status | Meaning |
|---|---|
| `202` | Still waiting for the person |
| `429` + `Retry-After` | Polled faster than `interval` |
| `200` | `{ credential, expires_at, email }`, the same shape a login enrolment returns. Answered **once**, then the flow is gone |
| `403` | Denied at the IdP, or refused by the binding. The body says which, in words a person can act on |
| `410` | Expired, or already collected |

### Where a pending flow lives

**In memory.** A flow lives ten minutes at most, and Silo is one process. A
restart that drops pending flows costs the person one retried login; a table
costs a migration and a sweeper, for state nothing else ever reads. Flows are
keyed by the SHA-256 of the poll token, so a heap dump holds no presentable
secret.

Each flow runs **one goroutine** calling `oauth2.Config.DeviceAccessToken`
with a context that ends at `expires_in`. That library call handles `interval`,
`slow_down` and `authorization_pending` correctly. Those are the subtle parts
of RFC 8628, and hand-rolling them is how an integration gets rate-limited by
the IdP. A flow nobody collects keeps polling the IdP until it expires, which
a cap bounds.

**The credential is minted when it is collected, not when the IdP answers.**
The goroutine verifies the token, runs the binding, and keeps only the account
id. The `Credential` row is written by the poll that returns it. Minting early
would leave a live credential in memory, and in the table, for a client that
may never come back for it.

**Bounded**, because every start costs an IdP request and a goroutine: a
per-IP bucket on `auth/device` in `loginlimit.go` beside the others, and a
process-wide cap on pending flows (256), past which new starts get `503`. The
poll token is 256 bits and needs no bucket of its own.

### Configuration and startup

```
SILO_OIDC_ISSUER=https://id.example.com/application/o/silo/
SILO_OIDC_CLIENT_ID=silo
SILO_OIDC_CLIENT_SECRET=…          # in silo.env, which the packages install 0600
SILO_OIDC_ACCOUNTS=link           # link (default) | create | isolated
SILO_OIDC_ALLOWED_DOMAINS=example.com,example.org   # optional
```

These are also accepted as an `[oidc]` section in `silo.conf`, through the same
`fromEnv` parsers, as every other option. Issuer, client id and secret come as
a set: one of the three without the others refuses to start, because half an
OIDC configuration is a typo. So does an unknown policy, and `create` or
`isolated` against a known public issuer with no domain list.

**Discovery is lazy and retried; it does not stop startup.** An IdP that is
down at boot must not take the file server down with it. Password login and
every existing credential still work, and they are what people are using. The
provider is fetched on the first `auth/device` and retried with backoff after
a failure. `server-info` advertises `oidc` whenever OIDC is *configured*, not
when the IdP answers, so a client shows the right first screen during an
outage and reports the `503` when it arrives.

`server-info` gains the feature name `oidc` (`POST auth/device`, `POST
auth/device/poll`). It is conditional, as `notifications` is. Nothing else
changes on that response.

### What does not change

- **No new credential kind.** An OIDC enrolment mints `session` or `device`,
  exactly as a password login does. It lists, labels, renews and revokes
  through the routes already built. `Resolve` is untouched.
- **No HTML, no redirect URI, no cookie.** The IdP never calls Silo in this
  plan. Backchannel logout, the one endpoint where it would, is a later step.
- **Password login stays on.** `SILO_PASSWORD_LOGIN=off` is designed in
  auth.md and deliberately later. It needs its outage answer (`silo user
  passwd` on the host) written down and tested first.

## Steps

Each step lands on its own, with the suite green. Where a step fixes a hazard
(the two discoveries at the top), the test comes first and is watched failing.

### Step 0 — arrival is one question

`account.Arrived(ctx, id)`: a password row or an identity row.
`invite.refuseEnrolled` asks it instead of `PasswordHash`. Test first: an
account holding only an `AccountIdentity` row (inserted directly, since
nothing can write one yet), deactivated, with an invite minted for its
address. Redemption succeeds today, and the test watches that happen before
the fix.

### Step 1 — a fake IdP to test against

`internal/oidctest`: an `httptest` server that serves discovery and JWKS, runs
a device authorization endpoint and a token endpoint, and signs ID tokens with
a key it generated. It is scriptable per test: pending *n* times, then approve
as `{sub, email, email_verified}`, deny, expire, `slow_down`, or sign with the
wrong key, wrong `aud`, wrong `iss`, or expired. Every later step is tested
against it, and none of them talks to a real IdP in CI.

It is a step of its own because it is the largest single piece of test code
here. A fake IdP that is subtly lenient makes every test that uses it pass for
the wrong reason. Its own tests check that it *refuses* what a real IdP
refuses.

### Step 2 — configuration and the provider

Adds `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` to `go.mod`. It
also adds the options, the all-three-or-none startup check, `fileserver/oidc`
holding the lazily discovered provider and verifier, and the conditional
`oidc` feature. Verification checks signature, `iss`, `aud == client_id`,
`exp` and `iat` through go-oidc's verifier, configured and not re-implemented.
Tests: a verifier accepts the fake IdP's good token and refuses each bad one;
startup refuses a partial configuration; `server-info` lists `oidc` only when
it is configured; an unreachable issuer at boot does not stop the server.

### Step 3 — the binding

`oidc.Bind(ctx, claims, policy) (account.ID, error)`, a pure function of the
database and the claims, tested with no HTTP at all. There is one test per row
of the rules above. These are the ones that must fail first against a naive
implementation:

- under `isolated`, an identity asserting an existing account's verified
  address is refused, not handed that account (the `created == false` hole);
- an address outside `SILO_OIDC_ALLOWED_DOMAINS` is refused under every
  policy, including for an identity that is already linked;
- an unverified address, and a token with no `email_verified` at all, match
  nothing and provision nothing;
- a linked but deactivated account is refused, not reactivated;
- a tombstone with an invite is claimed with the invite's role, and the
  invite reads as redeemed;
- two flows completing at once for the same new `sub` produce one account.
  `AccountIdentity`'s primary key decides the race, as `AccountEmail`'s does
  for addresses.

The PKCE correction in auth.md is made in this step.

### Step 4 — the endpoints

The two handlers, the flow table, the goroutine, the bucket and the cap, and
the routes. `auth_gate_wire_test.go` walks the router, and these two routes
join its list of deliberately unauthenticated routes, each with a comment
saying why. End-to-end tests against the fake IdP:

- start → pending → approve → `200` with a credential that then lists
  libraries;
- a second collection of the same poll token is `410`;
- polling faster than `interval` is `429`;
- denied is `403`, and so is a binding refusal, whose body is the binding's
  message;
- expiry is `410`, and the goroutine has exited (checked, not assumed:
  leaking one goroutine per abandoned login is the bug this would ship);
- `409` while setup is required, and `404` with OIDC unconfigured;
- the credential's label is the `client_name` that was sent, and `perm: "r"`
  yields a credential that cannot write.

### Step 5 — the CLI

`silo login` runs the device flow when the server lists `oidc`, or asks for a
password (`-password` forces it) when it does not. It prints the code and URL,
and stores the **device** credential it collects under the XDG config
directory, mode 0600, keyed by server URL, together with a client id minted
once per host. Signing in again discards the credential it replaces. `cli.Run`
uses `SILO_EMAIL` and `SILO_PASSWORD` when both are set, so a script that
signs in with a password behaves as before, and the stored credential
otherwise. `silo logout` revokes it through `auth/logout` and forgets it.

A command running on a stored credential does not sign it out afterwards.
`silo credential` used to, correctly, because every CLI session was a
throwaway; with a stored credential that would sign the host out as a side
effect of looking at a list.

This fixes the leftover credential-management.md recorded for anyone who uses
`silo login`: with a stored credential, `cli.Run` stops minting a 24-hour
session for every `silo ls`. A script that still signs in with a password
still mints one per command.

### Step 6 — what an administrator sees

`silo user list` gains an IDP column (issuer hosts) and `identities` in its
JSON. `silo user identity list <email>` shows them in full, and `link` and
`unlink <email> <issuer> <subject>` change them by hand. Linking is how an
existing account is reached under `isolated`. Linking an identity another
account holds is refused: one identity is one person.

**Unlinking an account's last way in is refused.** An account with no password
whose only identity goes would have neither thing `account.Arrived` asks about,
so it would read as a tombstone — which an invite, or an IdP login at its
address, may claim, libraries and all. Set a password or link another identity
first.

The changing-`sub` problem auth.md warns about turns out to need no manual
repair under `link` or `create`: the old identity row still marks the account
as arrived, so a login with the new subject and the same verified address is
linked beside it (and logged as a warning). Under `isolated`, it is `silo user
identity link` with the new subject.

`GET /account` gains `has_password`, so a client hides "change password" for an
account with nothing to change. A new key on an existing response, which is
what `docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md`
is about: checked rather than assumed, all three drive clients tolerate it —
Linux decodes into a struct, Android sets `ignoreUnknownKeys`, and macOS does
not call the endpoint.

### Step 7 — against a real IdP

Not in CI. Before this is called done, run it once by hand against Authentik
or Keycloak in a container, and against Clinch. The fake IdP proves Silo's
half. Only a real one proves the two halves agree about discovery, the device
page, and `email_verified`. Check the changing-`sub` behaviour auth.md warns
about on each of them, since the failure is silent.

Then auth.md's OIDC section moves from Part 2 to Part 1, with the corrections
above folded in. The roadmap entry closes, and `docs/protocol.md` documents
the two endpoints.

## Not in this plan

- **The drive clients.** They are a code screen and two endpoints in
  `silo-drive-linux` and `silo-drive-macos`, and they follow step 4. Each
  checks for `oidc` in `server-info`, shows the code with the URL (and a QR
  code), opens `verification_uri_complete` in the local browser when there is
  one, and polls.
- **Backchannel logout.** `credential.RevokeKind` was kept for it. It is the
  first endpoint the IdP calls directly, and it deserves a change of its own.
- **`SILO_PASSWORD_LOGIN=off`**, for the reason given above.
- **E2EE for OIDC-only accounts.** `account.SetKeys` already refuses an
  account with no password. A person who wants an encrypted library needs a
  password, and an OIDC-only account cannot set one, because `auth/password`
  asks for the current one. That gap is real and belongs to
  [`e2ee-completion.md`](e2ee-completion.md), which already lists it.
- **The authorization-code fallback** for an IdP without RFC 8628. Not built
  until such an IdP is met.
- **Linking an account to an IdP from a signed-in session.** See "Who gets an
  account"; `silo user identity link` covers it until then.
