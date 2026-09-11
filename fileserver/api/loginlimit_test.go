package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/ratelimit"
)

// resetLoginLimiters gives each test its own buckets — they are process
// globals, and a test that inherited another's failures would pass or fail by
// accident.
func resetLoginLimiters(t *testing.T, capacity int, refill time.Duration) {
	t.Helper()

	origIP, origAccount := loginIPLimiter, loginAccountLimiter
	origEnabled, origTrust := option.LoginRateLimit, option.TrustProxyHeaders
	t.Cleanup(func() {
		loginIPLimiter, loginAccountLimiter = origIP, origAccount
		option.LoginRateLimit, option.TrustProxyHeaders = origEnabled, origTrust
	})

	loginIPLimiter = ratelimit.New(capacity, refill)
	loginAccountLimiter = ratelimit.New(capacity, refill)
	option.LoginRateLimit = true
	option.TrustProxyHeaders = false
}

// loginAllowed runs one attempt through the limiter and releases its slot the
// moment it has the answer, the way a handler does when its verification
// returns. The tests below are about what accumulates across attempts; what one
// attempt holds while it is running is the limiter package's own test.
func loginAllowed(w http.ResponseWriter, r *http.Request, account string) bool {
	release, ok := allowLoginAttempt(w, r, account)
	release()
	return ok
}

func newLoginReq(remoteAddr, forwardedFor string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/silo/v1/auth/login", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	return req
}

// Both login endpoints ran authmgr.ValidatePassword with nothing in front of
// them, so guessing went as fast as the server could hash and each success
// minted a durable token.
func TestLoginAttemptsAreThrottledAfterFailures(t *testing.T) {
	resetLoginLimiters(t, 3, time.Minute)

	for i := 0; i < 3; i++ {
		req := newLoginReq("192.0.2.10:5000", "")
		if !loginAllowed(httptest.NewRecorder(), req, "bob@example.com") {
			t.Fatalf("attempt %d was blocked, want the first 3 allowed", i+1)
		}
		loginFailed(req, "bob@example.com")
	}

	rr := httptest.NewRecorder()
	if loginAllowed(rr, newLoginReq("192.0.2.10:5000", ""), "bob@example.com") {
		t.Fatal("a fourth attempt was allowed after three failures")
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusTooManyRequests)
	}
	retry := rr.Header().Get("Retry-After")
	if n, err := strconv.Atoi(retry); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retry)
	}
}

// A successful login clears the account's bucket, so someone who mistyped
// twice before getting it right is not left one slip from a 429.
func TestSuccessfulLoginClearsTheAccountBucket(t *testing.T) {
	resetLoginLimiters(t, 3, time.Hour)

	home := newLoginReq("192.0.2.10:5000", "")
	loginFailed(home, "bob@example.com")
	loginFailed(home, "bob@example.com")
	loginSucceeded("bob@example.com")

	// From an address with a full bucket, the account must have all three of
	// its tokens back. Without the reset it would have one, and the second
	// attempt here would be refused.
	laptop := newLoginReq("198.51.100.20:5000", "")
	for i := 0; i < 3; i++ {
		if !loginAllowed(httptest.NewRecorder(), laptop, "bob@example.com") {
			t.Fatalf("attempt %d was blocked after a successful login", i+1)
		}
		loginFailed(laptop, "bob@example.com")
	}

	// The address bucket is deliberately not reset by a success: an attacker
	// holding one valid credential would otherwise clear it at will. The two
	// failures from that address plus the three just made against the account
	// leave nothing.
	if loginAllowed(httptest.NewRecorder(), home, "bob@example.com") {
		t.Error("the buckets were reset by a successful login")
	}
}

// Guessing one account from one host must not throttle a different account
// from a different host.
func TestThrottlingIsPerAddressAndPerAccount(t *testing.T) {
	resetLoginLimiters(t, 2, time.Hour)

	attacker := newLoginReq("192.0.2.66:5000", "")
	for i := 0; i < 2; i++ {
		loginFailed(attacker, "victim@example.com")
	}
	if loginAllowed(httptest.NewRecorder(), attacker, "victim@example.com") {
		t.Fatal("the attacker was not throttled")
	}

	// Same account, different address: the per-account bucket still holds.
	if loginAllowed(httptest.NewRecorder(), newLoginReq("198.51.100.5:5000", ""), "victim@example.com") {
		t.Error("a distributed attack on one account was not throttled")
	}
	// Different account, different address: unaffected.
	if !loginAllowed(httptest.NewRecorder(), newLoginReq("198.51.100.5:5000", ""), "someone@example.com") {
		t.Error("an unrelated user was throttled")
	}
}

// Anyone can send X-Forwarded-For. Believing it unconditionally would hand an
// attacker a fresh address bucket on every request.
func TestForgedForwardedForDoesNotEvadeTheLimit(t *testing.T) {
	resetLoginLimiters(t, 2, time.Hour)

	for i := 0; i < 2; i++ {
		loginFailed(newLoginReq("192.0.2.66:5000", "10.0.0."+strconv.Itoa(i)), "bob@example.com")
	}

	req := newLoginReq("192.0.2.66:5000", "10.0.0.99")
	if loginAllowed(httptest.NewRecorder(), req, "someone-else@example.com") {
		t.Error("a forged X-Forwarded-For evaded the per-address limit")
	}

	// With the headers trusted — the reverse-proxy deployment — the forwarded
	// address is what counts, so a different one gets its own bucket.
	option.TrustProxyHeaders = true
	if !loginAllowed(httptest.NewRecorder(), req, "someone-else@example.com") {
		t.Error("a distinct forwarded address shared a bucket when the headers are trusted")
	}
}

// Case only changes the spelling of an account, not which account it is:
// ValidatePassword retries a miss with the email lowercased.
func TestAccountBucketFoldsCase(t *testing.T) {
	resetLoginLimiters(t, 2, time.Hour)

	for i := 0; i < 2; i++ {
		loginFailed(newLoginReq("192.0.2."+strconv.Itoa(i)+":5000", ""), "Bob@Example.com")
	}
	if loginAllowed(httptest.NewRecorder(), newLoginReq("198.51.100.7:5000", ""), "bob@example.com") {
		t.Error("re-spelling the account with different case evaded the limit")
	}
}

func TestRateLimitCanBeDisabled(t *testing.T) {
	resetLoginLimiters(t, 1, time.Hour)
	option.LoginRateLimit = false

	req := newLoginReq("192.0.2.10:5000", "")
	for i := 0; i < 50; i++ {
		loginFailed(req, "bob@example.com")
		if !loginAllowed(httptest.NewRecorder(), req, "bob@example.com") {
			t.Fatalf("attempt %d was blocked with rate limiting disabled", i+1)
		}
	}
}

// The slot is held across the verification rather than let go the moment it is
// granted. That is the whole point of reserving: ValidatePassword is 600k
// PBKDF2 iterations, and a check that held nothing left all of it as a window
// in which every other arrival read the same unspent bucket.
func TestConcurrentLoginAttemptsCannotExceedCapacity(t *testing.T) {
	const capacity = 3
	const attempts = 200
	resetLoginLimiters(t, capacity, time.Minute)

	var reached atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := newLoginReq("192.0.2.10:5000", "")
			release, ok := allowLoginAttempt(httptest.NewRecorder(), req, "bob@example.com")
			if !ok {
				return
			}
			defer release()
			reached.Add(1)
			time.Sleep(20 * time.Millisecond) // the hash
			loginFailed(req, "bob@example.com")
		}()
	}
	close(start)
	wg.Wait()

	if n := reached.Load(); n > capacity {
		t.Errorf("%d of %d concurrent attempts reached the hash, against a capacity of %d", n, attempts, capacity)
	}
}
