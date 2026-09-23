package api

// Signing in through the identity provider: the two endpoints of the device
// grant as a client sees it.
//
// The client never speaks to the IdP. It asks this server to start a login and
// is handed the IdP's code and URL to show; the person approves at the IdP on
// whatever browser they like; this server polls the IdP, verifies what comes
// back and decides which account it is; and the client, which has been polling
// this server meanwhile, collects an ordinary Credential. docs/auth.md § OIDC is
// the design and docs/plans/oidc.md the decisions. The two that shape this file:
//
//   - A pending login lives in memory. It lasts minutes, this is one process,
//     and a restart that drops one costs somebody a retried sign-in; a table
//     would cost a migration and a sweeper for state nothing else reads.
//   - The credential is minted when it is collected, not when the IdP answers.
//     The flow holds an account id until the client comes back for it, so a
//     client that never does leaves nothing live behind -- no row, and no
//     secret sitting in memory.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/oidc"
	"github.com/SiloDrive/silo/fileserver/oidc/bind"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/fileserver/setup"
	log "github.com/sirupsen/logrus"
)

// maxPendingDeviceFlows bounds what starting logins can cost: each pending one
// is a goroutine polling the IdP until its code expires. The per-address bucket
// keeps one host from filling it; this keeps many from filling the process.
var maxPendingDeviceFlows = 256

// deviceWaiters counts the goroutines polling the IdP, so a test can see that
// an abandoned login's goroutine ends rather than assume it.
var deviceWaiters atomic.Int64

type deviceState int

const (
	devicePending deviceState = iota
	deviceApproved
	deviceRefused
	deviceExpired
)

type deviceFlow struct {
	// Fixed at start.
	opts     credential.IssueOpts
	interval time.Duration
	expires  time.Time

	mu       sync.Mutex
	state    deviceState
	lastPoll time.Time
	account  account.ID
	refusal  error
}

var deviceFlows = struct {
	sync.Mutex
	m map[[sha256.Size]byte]*deviceFlow
}{m: map[[sha256.Size]byte]*deviceFlow{}}

// pollKey is how a flow is found. The table holds the hash of the poll token
// rather than the token, so nothing in memory can be presented back.
func pollKey(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// addDeviceFlow registers a flow, or reports that the table is full. Expired
// flows are swept here rather than on a ticker: the table only needs to be
// tidy at the moment it might be full, and a ticker would be a goroutine for
// the life of every process, most of which never see a device login.
func addDeviceFlow(key [sha256.Size]byte, f *deviceFlow) bool {
	deviceFlows.Lock()
	defer deviceFlows.Unlock()
	now := time.Now()
	for k, old := range deviceFlows.m {
		if now.After(old.expires) {
			delete(deviceFlows.m, k)
		}
	}
	if len(deviceFlows.m) >= maxPendingDeviceFlows {
		return false
	}
	deviceFlows.m[key] = f
	return true
}

func lookupDeviceFlow(key [sha256.Size]byte) *deviceFlow {
	deviceFlows.Lock()
	defer deviceFlows.Unlock()
	return deviceFlows.m[key]
}

// takeDeviceFlow removes a flow and reports whether this caller removed it.
// It is what makes the credential collectable once: two polls racing for an
// approved flow both reach here, and one of them gets false.
func takeDeviceFlow(key [sha256.Size]byte, f *deviceFlow) bool {
	deviceFlows.Lock()
	defer deviceFlows.Unlock()
	if deviceFlows.m[key] != f {
		return false
	}
	delete(deviceFlows.m, key)
	return true
}

// deviceStartRequest is the enrolment half of a login request, unchanged, so
// the two ways of getting a credential cannot disagree about what one may ask
// for: both go through enrolmentOpts.
type deviceStartRequest struct {
	Kind       string `json:"kind"`
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id"`
	Perm       string `json:"perm"`
	Scope      string `json:"scope"`
}

type deviceStartResponse struct {
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	PollToken               string `json:"poll_token"`
	Interval                int64  `json:"interval"`
	ExpiresIn               int64  `json:"expires_in"`
}

// DeviceStartHandler handles POST /api/silo/v1/auth/device.
func DeviceStartHandler(w http.ResponseWriter, r *http.Request) {
	client := oidc.Current()
	if client == nil {
		// The oidc feature is absent from server-info for the same reason, so
		// a client that checked first never sees this.
		http.Error(w, "This server does not sign in through an identity provider", http.StatusNotFound)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()
	// A server nobody has claimed is claimed with the setup token. Letting the
	// first person through the IdP's device page arrive first would make them
	// the administrator by being quick.
	if required, err := setup.Required(ctx); err != nil {
		log.Errorf("Failed to check whether setup is required: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	} else if required {
		http.Error(w, "This server has not been set up yet", http.StatusConflict)
		return
	}

	if !allowDeviceStart(w, r) {
		return
	}

	var req deviceStartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Required here as it is for a durable credential from a password: a
	// credential nobody can tell apart from four others turns revocation into
	// a guess. enrolmentOpts asks too, but only once it knows this is an
	// enrolment, and every device login is one.
	if req.ClientName == "" {
		http.Error(w, "client_name is required", http.StatusBadRequest)
		return
	}
	opts, ok := enrolmentOpts(w, loginRequest{
		Kind: req.Kind, ClientName: req.ClientName, ClientID: req.ClientID, Perm: req.Perm, Scope: req.Scope,
	})
	if !ok {
		return
	}

	token, err := newPollToken()
	if err != nil {
		log.Errorf("Failed to mint a poll token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Checked before asking the IdP, so a full table costs the IdP nothing.
	// Checked again when the flow is added, which is the check that counts.
	deviceFlows.Lock()
	full := len(deviceFlows.m) >= maxPendingDeviceFlows
	deviceFlows.Unlock()
	if full {
		http.Error(w, "Too many sign-ins in progress; try again shortly", http.StatusServiceUnavailable)
		return
	}

	da, err := client.Start(r.Context())
	if err != nil {
		log.Warnf("Device sign-in could not start: %v", err)
		http.Error(w, "The identity provider is unavailable", http.StatusServiceUnavailable)
		return
	}

	interval := da.Interval
	if interval <= 0 {
		interval = 5 // RFC 8628 § 3.2's default
	}
	expires := da.Expiry
	if expires.IsZero() {
		expires = time.Now().Add(10 * time.Minute)
	}
	f := &deviceFlow{opts: opts, interval: time.Duration(interval) * time.Second, expires: expires}
	key := pollKey(token)
	if !addDeviceFlow(key, f) {
		http.Error(w, "Too many sign-ins in progress; try again shortly", http.StatusServiceUnavailable)
		return
	}

	// Not the request's context: the request ends now, and the wait runs
	// until the person approves or the code expires.
	waitCtx, cancelWait := context.WithDeadline(context.Background(), expires)
	deviceWaiters.Add(1)
	go func() {
		defer deviceWaiters.Add(-1)
		defer cancelWait()
		waitForApproval(waitCtx, client, da, f)
	}()

	writeJSON(w, http.StatusOK, deviceStartResponse{
		UserCode:                da.UserCode,
		VerificationURI:         da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete,
		PollToken:               token,
		Interval:                interval,
		ExpiresIn:               int64(time.Until(expires).Seconds()),
	})
}

// waitForApproval is the goroutine behind one pending login: it waits out the
// IdP, decides the account, and records the outcome for the client's poll.
func waitForApproval(ctx context.Context, client *oidc.Client, da *oidc.Pending, f *deviceFlow) {
	claims, err := client.Wait(ctx, da)
	if err != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		if errors.Is(err, oidc.ErrExpired) {
			f.state = deviceExpired
			return
		}
		f.state, f.refusal = deviceRefused, err
		if !errors.Is(err, oidc.ErrDenied) {
			log.Warnf("Device sign-in failed: %v", err)
		}
		return
	}

	dbCtx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	id, err := bind.Account(dbCtx, &client.Config, claims)

	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.state, f.refusal = deviceRefused, err
		log.Infof("Device sign-in for %s subject %s (%s) refused: %v", claims.Issuer, claims.Subject, claims.Email, err)
		return
	}
	f.state, f.account = deviceApproved, id
}

type devicePollRequest struct {
	PollToken string `json:"poll_token"`
}

// DevicePollHandler handles POST /api/silo/v1/auth/device/poll.
//
// Status codes carry the answer, the way the rest of this API's do, rather than
// RFC 8628's error strings in a 400: 202 still waiting, 429 with Retry-After
// for polling too fast, 200 with the credential once, 403 refused, 410 gone.
func DevicePollHandler(w http.ResponseWriter, r *http.Request) {
	var req devicePollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key := pollKey(req.PollToken)
	f := lookupDeviceFlow(key)
	if f == nil {
		// An unknown token and a collected or swept one are the same answer.
		// There is nothing to distinguish them for: both mean start again.
		http.Error(w, "This sign-in has expired or has already been completed", http.StatusGone)
		return
	}

	f.mu.Lock()
	now := time.Now()
	if now.After(f.expires) && f.state == devicePending {
		f.state = deviceExpired
	}
	state, refusal, id := f.state, f.refusal, f.account
	// The interval is enforced only while there is nothing to report. A
	// client that polls slightly early on the poll that would have succeeded
	// should get its answer, not a reason to wait again.
	tooSoon := state == devicePending && !f.lastPoll.IsZero() &&
		now.Sub(f.lastPoll) < f.interval-100*time.Millisecond
	if !tooSoon {
		f.lastPoll = now
	}
	f.mu.Unlock()

	switch state {
	case devicePending:
		if tooSoon {
			w.Header().Set("Retry-After", strconv.Itoa(int(f.interval.Seconds())))
			http.Error(w, "Polling faster than the interval", http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	case deviceExpired:
		takeDeviceFlow(key, f)
		http.Error(w, "This sign-in expired before it was approved", http.StatusGone)
		return
	case deviceRefused:
		takeDeviceFlow(key, f)
		http.Error(w, refusalMessage(refusal), http.StatusForbidden)
		return
	}

	// Approved. Taking the flow out of the table first is what makes this
	// once: a second poll racing this one finds nothing and is told so.
	if !takeDeviceFlow(key, f) {
		http.Error(w, "This sign-in has expired or has already been completed", http.StatusGone)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()
	// Asked again at collection: an administrator may have switched the
	// account off in the minutes since the IdP answered, and a credential
	// for it would resolve to nothing anyway.
	acct, err := account.ByID(ctx, id)
	if err != nil {
		log.Errorf("Device sign-in approved for %s, which could not be read back: %v", id, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !acct.IsActive {
		http.Error(w, refusalMessage(bind.ErrDisabled), http.StatusForbidden)
		return
	}

	opts := f.opts
	opts.AccountID = id
	cred, token, err := credential.Issue(ctx, opts)
	if err != nil {
		log.Errorf("Failed to issue a %s credential for a device sign-in: %v", opts.Kind, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Infof("Device sign-in: %s credential %q issued to %s", opts.Kind, opts.Label, acct.Email)
	writeJSON(w, http.StatusOK, enrolmentResponse{Credential: token, ExpiresAt: cred.ExpiresAt, Email: acct.Email})
}

// refusalMessage is what the person is told. The binding's refusals are written
// to be read by them; anything else is the IdP's business and is said without
// the detail, which is in the log.
func refusalMessage(err error) string {
	for _, known := range []error{
		bind.ErrDomain, bind.ErrDisabled, bind.ErrNoAccount, bind.ErrAddressTaken, bind.ErrNoVerifiedAddress,
	} {
		if errors.Is(err, known) {
			return capitalise(known.Error())
		}
	}
	if errors.Is(err, oidc.ErrDenied) {
		return "The sign-in was denied at the identity provider"
	}
	return "The identity provider's answer could not be verified"
}

func capitalise(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// newPollToken is 256 bits, which is why the poll endpoint has no bucket of its
// own: nobody guesses one.
func newPollToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
