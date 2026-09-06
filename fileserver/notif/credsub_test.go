package notif

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/middleware"
)

// authorizeReturning replaces the authorizer for one test.
//
// It is a variable so this package can ask the permission question without
// owning the answer: share.CheckPerm needs a database, and every test here
// would otherwise need one to assert something that is not about storage.
func authorizeReturning(t *testing.T, fn func(*credential.Credential, string) bool) {
	t.Helper()
	orig := authorize
	authorize = fn
	t.Cleanup(func() { authorize = orig })
}

// dialWithCredential opens a socket authenticated the way the API routes are.
func dialWithCredential(t *testing.T, cred *credential.Credential) *websocket.Conn {
	t.Helper()
	acct := &account.Account{Email: "watcher@example.com", IsActive: true}
	return dialSocket(t, func(r *http.Request) *http.Request {
		return middleware.WithCredential(r, cred, acct)
	})
}

func testCredential() *credential.Credential {
	return &credential.Credential{
		ID:        "cred-1",
		Kind:      credential.KindDevice,
		AccountID: account.ID{1},
		Perm:      "rw",
	}
}

// subscribeByCredentialTo sends a subscribe frame carrying no token.
func subscribeByCredentialTo(t *testing.T, conn *websocket.Conn, libraryID string) {
	t.Helper()
	sendSubscribe(t, conn, subscribeLibrary{LibraryID: libraryID})
}

// A subscribe frame with no token is authorized by the socket's credential.
//
// This is the point of the change: /notification is the last endpoint that
// authenticates by a minted, library-scoped bearer token instead of by the
// Authorization header every other route reads. The notification server was a
// separate process with no database, so the answer had to arrive pre-signed;
// it runs in-process now and can simply ask.
func TestASubscribeWithNoTokenIsAuthorizedByTheCredential(t *testing.T) {
	const libraryID = testLibrary
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	authorizeReturning(t, func(*credential.Credential, string) bool { return true })

	conn := dialWithCredential(t, testCredential())
	subscribeByCredentialTo(t, conn, libraryID)

	// The subscribe is handled on the read loop, so give it a moment to land
	// before the fanout snapshots subscribers.
	waitForSubscribers(t, libraryID, 1)

	NotifyLibraryUpdate(libraryID, commitID)
	ev := awaitUpdate(t, conn, libraryID)
	if ev.CommitID != commitID {
		t.Errorf("got commit %q, want %q", ev.CommitID, commitID)
	}
}

// A library the credential may not reach is refused, and said so.
//
// Silence would be worse than a refusal: the client believes it is subscribed
// and stops polling, so a permission answer it never hears becomes a library
// that appears to have stopped changing.
func TestASubscribeWithNoTokenIsRefusedWhenTheCredentialCannotReachTheLibrary(t *testing.T) {
	const libraryID = testLibrary

	authorizeReturning(t, func(*credential.Credential, string) bool { return false })

	conn := dialWithCredential(t, testCredential())
	subscribeByCredentialTo(t, conn, libraryID)

	msg := awaitFrame(t, conn, EventTypeSubscribeDenied, 2*time.Second)
	var denied map[string]string
	if err := json.Unmarshal(msg.Content, &denied); err != nil {
		t.Fatalf("bad subscribe-denied content: %v", err)
	}
	if denied["library_id"] != libraryID {
		t.Errorf("denied names library %q, want %q", denied["library_id"], libraryID)
	}

	if n := subscriberCount(libraryID); n != 0 {
		t.Errorf("a refused subscribe left %d subscriber(s) behind", n)
	}
}

// An anonymous socket cannot subscribe on a credential it does not have.
//
// The token lane was the only thing an anonymous connection could prove
// anything with. Letting a bare library id through here would turn the new
// lane into an unauthenticated subscribe to any library id a stranger can
// guess.
func TestASubscribeWithNoTokenIsRefusedOnAnAnonymousSocket(t *testing.T) {
	const libraryID = testLibrary

	// Would say yes if it were ever asked. It must not be asked.
	authorizeReturning(t, func(*credential.Credential, string) bool {
		t.Error("the authorizer was consulted for a socket with no credential")
		return true
	})

	conn := dialSocket(t, nil)
	subscribeByCredentialTo(t, conn, libraryID)

	awaitFrame(t, conn, EventTypeSubscribeDenied, 2*time.Second)
	if n := subscriberCount(libraryID); n != 0 {
		t.Errorf("an anonymous subscribe left %d subscriber(s) behind", n)
	}
}

// A credential-authorized subscription is not swept as a stale token.
//
// It carries no expiry of its own, and the sweep reads a zero exp as "expired
// in 1970" unless it is told otherwise -- which would drop every subscription
// on the new lane an hour after it was made.
func TestACredentialAuthorizedSubscriptionSurvivesTheSweep(t *testing.T) {
	const libraryID = testLibrary
	Init()

	authorizeReturning(t, func(*credential.Credential, string) bool { return true })

	c := fakeClient()
	c.cred = testCredential()
	c.subscribe(libraryID, "", subscription{lane: laneCredential})

	c.sweepSubscriptions()

	if n := subscriberCount(libraryID); n != 1 {
		t.Errorf("the sweep dropped a credential-authorized subscription: %d subscriber(s) left", n)
	}
}

// Access withdrawn under a live socket drops the subscription on the next sweep.
//
// The token lane bounded this at 72 hours by expiry alone. A credential that
// never expires would otherwise hold a subscription to a library its account
// was un-shared from for the life of the process, and the frames it receives
// carry a commit id.
func TestASweepDropsASubscriptionTheCredentialNoLongerReaches(t *testing.T) {
	const libraryID = testLibrary
	Init()

	allowed := true
	authorizeReturning(t, func(*credential.Credential, string) bool { return allowed })

	c := fakeClient()
	c.cred = testCredential()
	c.subscribe(libraryID, "", subscription{lane: laneCredential})

	allowed = false
	c.sweepSubscriptions()

	if n := subscriberCount(libraryID); n != 0 {
		t.Errorf("a subscription outlived the access that authorized it: %d subscriber(s) left", n)
	}
	expectQueued(t, c, EventTypeSubscribeDenied)
}

// A token-authorized subscription is still swept on its expiry.
//
// The two lanes share one sweep, and the older one must keep working
// unchanged: this is the behaviour every client in the field depends on.
func TestTheSweepStillExpiresATokenAuthorizedSubscription(t *testing.T) {
	const libraryID = testLibrary
	Init()

	c := fakeClient()
	c.subscribe(libraryID, "alice@example.com", subscription{lane: laneToken, exp: time.Now().Add(-time.Minute).Unix()})

	c.sweepSubscriptions()

	if n := subscriberCount(libraryID); n != 0 {
		t.Errorf("an expired token kept its subscription: %d subscriber(s) left", n)
	}
	expectQueued(t, c, EventTypeJWTExpired)
}
