package api

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/ratelimit"
	"github.com/dkam/silo/fileserver/utils"
	log "github.com/sirupsen/logrus"
)

// Both login endpoints — /api/silo/v1/auth/login and /api2/auth-token/ — call
// authmgr.ValidatePassword with nothing in front of it, so password guessing
// ran as fast as the server could hash and every success minted a durable
// token. These are the buckets that stop it.
//
// Two dimensions, because either alone leaves a hole. Per address stops one
// host working through many accounts; per account stops many hosts working on
// one account. Only failures spend a token, so a user logging in repeatedly is
// never throttled.
//
// The per-account limit refills more slowly but from a bucket the same size,
// which bounds a distributed attack on one account at a guess a minute. It
// does mean an attacker can throttle a specific user by failing on their
// behalf. That is the standard cost of counting per account, and it is bounded
// — a minute's wait, not a lockout.
var (
	loginIPLimiter      = ratelimit.New(10, 30*time.Second)
	loginAccountLimiter = ratelimit.New(10, time.Minute)
)

// The pre-login KDF endpoint gets its own bucket, and it is a different shape
// from the two above.
//
// It is per address only, because there is no account to count against: the
// endpoint's whole contract is that it answers alike whether or not one
// exists, and a per-account bucket would break that -- an address that could
// be throttled is an address that has an account.
//
// Every request spends a token rather than only the failures, for the same
// reason: there is no failure. What this bounds is the rate at which one host
// can sweep addresses, and the cost of the sweep to this server; what makes
// the sweep useless is the indistinguishable answer, not this. Sixty a minute
// is far above what logging in needs -- one request per login -- and far below
// what enumerating a directory wants.
var kdfIPLimiter = ratelimit.New(60, time.Minute)

// The setup endpoint gets its own bucket, per address only.
//
// Not because eighty bits needs defending. A guess a millisecond for the age of
// the universe does not get through 2^80, so the token is the defence and this
// is not -- do not read the limiter as load-bearing and shorten the token.
// What this bounds is the cost of the endpoint to the server and the damage a
// client stuck in a retry loop can do, while still forgiving an operator
// retyping a code off a log line.
//
// Per address only, because there is no account to count against: the address
// in the request is one the operator is inventing, and a per-account bucket
// would be a fresh map entry for every string submitted -- unbounded growth
// keyed by attacker input, which is what the cleanup sweep exists to avoid.
//
// Smaller and slower than the login buckets, because the traffic is different.
// Login has to tolerate many legitimate users behind one address; this endpoint
// has exactly one legitimate user, once, ever.
var setupIPLimiter = ratelimit.New(5, time.Minute)

// StartLoginLimiterCleanup drops idle buckets for the life of the process.
func StartLoginLimiterCleanup() {
	loginIPLimiter.StartCleanup()
	loginAccountLimiter.StartCleanup()
	kdfIPLimiter.StartCleanup()
	setupIPLimiter.StartCleanup()
}

// ResetRateLimiters refills every bucket this package keeps.
//
// Exported for tests in other packages, which is a cost worth naming: these
// limiters are package-level state with a lifetime longer than any test, and
// the wire tests that drive the real router live in fileserver rather than
// here, so an export_test.go seam cannot reach them. Without it a test that
// exhausts a bucket deliberately — and one does — decides whether its
// neighbours pass, and the failure arrives as a 429 that reads like a product
// bug rather than a leak.
func ResetRateLimiters() {
	loginIPLimiter.ResetAll()
	loginAccountLimiter.ResetAll()
	kdfIPLimiter.ResetAll()
	setupIPLimiter.ResetAll()
}

// allowSetupAttempt reports whether a setup attempt may proceed, writing a 429
// itself when it may not. Only failures spend a token, matching login: the one
// success this endpoint ever sees should not leave the operator throttled.
func allowSetupAttempt(w http.ResponseWriter, r *http.Request) bool {
	if !option.LoginRateLimit {
		return true
	}
	ip := utils.ClientIP(r, option.TrustProxyHeaders)
	if ok, retry := setupIPLimiter.Allowed(ip); !ok {
		tooManyAttempts(w, "setup", retry)
		log.Warnf("Setup rate limit reached for address %s", ip)
		return false
	}
	return true
}

// setupFailed charges a refused setup attempt against the address bucket.
func setupFailed(r *http.Request) {
	if !option.LoginRateLimit {
		return
	}
	setupIPLimiter.Penalize(utils.ClientIP(r, option.TrustProxyHeaders))
}

// allowKDFRequest reports whether a pre-login parameter request may proceed,
// writing a 429 itself when it may not.
func allowKDFRequest(w http.ResponseWriter, r *http.Request) bool {
	if !option.LoginRateLimit {
		return true
	}
	ip := utils.ClientIP(r, option.TrustProxyHeaders)
	if ok, retry := kdfIPLimiter.Allowed(ip); !ok {
		tooManyAttempts(w, "pre-login parameter", retry)
		log.Warnf("Pre-login parameter rate limit reached for address %s", ip)
		return false
	}
	kdfIPLimiter.Penalize(ip)
	return true
}

// allowLoginAttempt reports whether a login attempt may proceed, writing a 429
// itself when it may not. Call it before validating the password: the point is
// to keep the verification, and the answer it leaks, out of reach.
func allowLoginAttempt(w http.ResponseWriter, r *http.Request, account string) bool {
	if !option.LoginRateLimit {
		return true
	}

	ip := utils.ClientIP(r, option.TrustProxyHeaders)
	if ok, retry := loginIPLimiter.Allowed(ip); !ok {
		tooManyAttempts(w, "login", retry)
		log.Warnf("Login rate limit reached for address %s", ip)
		return false
	}
	if ok, retry := loginAccountLimiter.Allowed(accountKey(account)); !ok {
		tooManyAttempts(w, "login", retry)
		log.Warnf("Login rate limit reached for account %s", account)
		return false
	}
	return true
}

// loginFailed charges a failed attempt against both buckets.
func loginFailed(r *http.Request, account string) {
	if !option.LoginRateLimit {
		return
	}
	loginIPLimiter.Penalize(utils.ClientIP(r, option.TrustProxyHeaders))
	loginAccountLimiter.Penalize(accountKey(account))
}

// loginSucceeded clears the account's bucket, so someone who mistyped their
// password twice before getting it right is not left one slip from a 429. The
// address bucket is deliberately left alone: an attacker holding one valid
// credential would otherwise be able to reset it at will.
func loginSucceeded(account string) {
	if !option.LoginRateLimit {
		return
	}
	loginAccountLimiter.Reset(accountKey(account))
}

// accountKey folds case so that Bob@example.com and bob@example.com are not
// two buckets to guess against. ValidatePassword retries a miss with the
// email lowercased, so both spellings reach the same account.
func accountKey(account string) string {
	return strings.ToLower(strings.TrimSpace(account))
}

// tooManyAttempts writes the 429 for whichever bucket refused, naming it. The
// body was "Too many login attempts" when logging in was the only thing behind
// a bucket; it takes the noun now because two of the three callers are not
// logins, and a setup attempt told it had made too many login attempts is being
// pointed at the wrong thing to stop doing.
func tooManyAttempts(w http.ResponseWriter, what string, retry time.Duration) {
	// Rounded up: Retry-After carries whole seconds, and a truncated wait
	// would invite a retry that is still too early.
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, "Too many "+what+" attempts", http.StatusTooManyRequests)
}
