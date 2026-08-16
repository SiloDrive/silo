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

// StartLoginLimiterCleanup drops idle buckets for the life of the process.
func StartLoginLimiterCleanup() {
	loginIPLimiter.StartCleanup()
	loginAccountLimiter.StartCleanup()
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
		tooManyAttempts(w, retry)
		log.Warnf("Login rate limit reached for address %s", ip)
		return false
	}
	if ok, retry := loginAccountLimiter.Allowed(accountKey(account)); !ok {
		tooManyAttempts(w, retry)
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

func tooManyAttempts(w http.ResponseWriter, retry time.Duration) {
	// Rounded up: Retry-After carries whole seconds, and a truncated wait
	// would invite a retry that is still too early.
	seconds := int(math.Ceil(retry.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, "Too many login attempts", http.StatusTooManyRequests)
}
