package notif

import (
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/credential"
)

// visibleLibrariesReturning replaces the visible-set query for one test.
//
// Owned and granted are two tables in two packages, and the socket's tests
// own neither. What is under test is what the socket does with the answer.
func visibleLibrariesReturning(t *testing.T, ids ...string) {
	t.Helper()
	visibleLibrariesFrom(t, func() []string { return ids })
}

// subscribeToAccount sends the frame that asks for the account's whole set.
func subscribeToAccount(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	content, err := json.Marshal(subscribeFrame{Account: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(&Message{Type: "subscribe", Content: content}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// expectNoFrame fails if anything at all arrives within d.
func expectNoFrame(t *testing.T, conn *websocket.Conn, d time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	var msg Message
	err := conn.ReadJSON(&msg)
	if err == nil {
		t.Fatalf("got a %s frame, want silence", msg.Type)
	}
	if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		t.Fatalf("waiting for silence: %v", err)
	}
}

// A scoped credential asking for the account's set gets its own library's
// ring, and not the account's.
//
// This is decisions 5 and 6 in one assertion. The scope is a ceiling below the
// account, so the credential cannot answer for the set -- and it does not have
// to, because the scope already names the one library it may watch. The frame
// is the same, and the credential decides what it gets. The set is never
// resolved: a query for libraries this credential may not be told about is a
// query for nothing.
func TestAScopedCredentialAskingForTheAccountGetsItsLibrarysRing(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	visibleLibrariesFrom(t, func() []string {
		t.Error("the account's set was resolved for a credential that cannot see it")
		return []string{testLibrary, other}
	})
	cred := testCredential()
	cred.Scope = credential.Scope{LibraryID: testLibrary}
	stillGood(t, cred)
	conn := dialWithCredential(t, cred)
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	if n := subscriberCount(other); n != 0 {
		t.Fatalf("a scoped credential was subscribed to a library outside its scope")
	}

	NotifyLibraryUpdate(testLibrary, "0123456789abcdef0123456789abcdef01234567")
	msg := nextFrame(t, conn, 2*time.Second)
	if msg.Type != EventTypeAccountUpdate {
		t.Fatalf("a scoped socket was sent %s, want %s", msg.Type, EventTypeAccountUpdate)
	}

	NotifyLibraryUpdate(other, "0123456789abcdef0123456789abcdef01234567")
	expectNoFrame(t, conn, 200*time.Millisecond)
}

// A path-scoped credential is rung for a commit elsewhere in its library.
//
// This pins decision 6's trade as deliberate. On the per-library lane the same
// credential is refused its library outright -- TestPermForAppliesTheNarrowing
// in middleware is the contrast -- because that lane's frame carries a commit
// id, and a folder scope cannot be answered about the library as a whole
// without telling the holder about paths it may not reach. A ring tells it
// nothing but "look", and the look is authorized on its own. Narrowing this
// later is a change to this test, not a silent one.
func TestAPathScopedCredentialIsRungForACommitElsewhereInItsLibrary(t *testing.T) {
	cred := testCredential()
	cred.Scope = credential.Scope{LibraryID: testLibrary, Path: "/photos"}
	stillGood(t, cred)
	conn := dialWithCredential(t, cred)
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	NotifyLibraryUpdate(testLibrary, "0123456789abcdef0123456789abcdef01234567")
	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
}

// A scoped socket closes when its credential's scope moves.
//
// The socket was granted one library's ring on the strength of a scope that
// named it. Re-read with a different scope -- or none -- the credential is
// not the one that subscribed, and the client reconnects to be answered by
// the one it now holds.
func TestAScopedSocketClosesWhenItsCredentialsScopeMoves(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	recheckReturning(t, func(string) (*credential.Credential, error) {
		cred := testCredential()
		cred.Scope = credential.Scope{LibraryID: other}
		return cred, nil
	})

	cred := testCredential()
	cred.Scope = credential.Scope{LibraryID: testLibrary}
	conn := dialWithCredential(t, cred)
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	snapshotSubscribers(testLibrary)[0].resyncAccount()

	if !closedWithin(t, conn, 2*time.Second) {
		t.Fatal("the socket stayed open on a credential whose scope had moved")
	}
}

// A rename rings a scoped socket too. Its set is the scope and cannot move,
// but the name is not in the set.
func TestARenameRingsAScopedSocket(t *testing.T) {
	cred := testCredential()
	cred.Scope = credential.Scope{LibraryID: testLibrary}
	stillGood(t, cred)
	conn := dialWithCredential(t, cred)
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	NotifyLibraryRenamed(testLibrary)
	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
}

// A commit to a library the account can see reaches an account-scoped socket
// that never named that library.
//
// This is the mode's whole point: the client subscribes once and is told about
// every library in its set, including the ones it has not heard of yet.
//
// What reaches it is a bare ring, not the library-update a per-library socket
// gets. The ring carries nothing, and that is what makes this lane need no
// lease: there is nothing in the frame to have been authorized, and the fetch
// it provokes is authorized on its own. A library-update here would carry a
// library id and a commit id on a socket that was never checked against that
// library in particular.
func TestACommitToAVisibleLibraryRingsAnAccountScopedSocket(t *testing.T) {
	const commitID = "0123456789abcdef0123456789abcdef01234567"
	visibleLibrariesReturning(t, testLibrary)

	conn, _ := liveAccountClient(t)

	NotifyLibraryUpdate(testLibrary, commitID)

	msg := nextFrame(t, conn, 2*time.Second)
	if msg.Type != EventTypeAccountUpdate {
		t.Fatalf("an account socket was sent %s, want %s", msg.Type, EventTypeAccountUpdate)
	}
	if string(msg.Content) != "{}" {
		t.Errorf("the ring carried %s; it must carry nothing", msg.Content)
	}
	expectNoFrame(t, conn, 200*time.Millisecond)
}

// Several commits an account socket could not be handed collapse into one ring.
//
// The per-library lane owes a commit id per library and reconciles them; this
// lane's debt is one bit. A ring is owed or it is not, and one ring after the
// queue moves says everything thirty would.
func TestDropsForAnAccountSocketCollapseIntoOneRing(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)

	conn, c := liveAccountClient(t)
	resume := stall(t, c)

	NotifyLibraryUpdate(testLibrary, "1111111111111111111111111111111111111111")
	NotifyLibraryUpdate(testLibrary, "2222222222222222222222222222222222222222")
	NotifyLibraryUpdate(testLibrary, "3333333333333333333333333333333333333333")

	resume()

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)

	// And no second ring. The filler stall queued is still draining, so read
	// past it rather than demanding silence.
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			return // the read deadline: nothing more came, which is the point
		}
		if msg.Type == EventTypeAccountUpdate {
			t.Fatal("a second ring arrived; the drops should have collapsed into one")
		}
	}
}

// liveAccountClient dials a socket with a credential, subscribes it to the
// account, and returns both ends once the server has registered it.
func liveAccountClient(t *testing.T) (*websocket.Conn, *Client) {
	t.Helper()
	conn := dialWithCredential(t, testCredential())
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)
	return conn, snapshotSubscribers(testLibrary)[0]
}

// nextFrame reads whatever arrives next, or fails at the deadline.
func nextFrame(t *testing.T, conn *websocket.Conn, d time.Duration) *Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	var msg Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("waiting for a frame: %v", err)
	}
	return &msg
}

// A commit to a library the account cannot see reaches it not at all.
//
// Every ring is gated on the same set the listing already answers with. A
// socket that heard about a library outside that set would be a listing that
// leaks, one library id at a time.
func TestACommitToAnInvisibleLibraryDoesNotReachAnAccountScopedSocket(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	visibleLibrariesReturning(t, testLibrary)

	conn, _ := liveAccountClient(t)

	if n := subscriberCount(other); n != 0 {
		t.Fatalf("an account subscribe registered %d subscriber(s) on a library outside its set", n)
	}
	NotifyLibraryUpdate(other, "0123456789abcdef0123456789abcdef01234567")
	expectNoFrame(t, conn, 200*time.Millisecond)
}

// visibleLibrariesFrom is the same for a set the test moves under the socket.
func visibleLibrariesFrom(t *testing.T, get func() []string) {
	t.Helper()
	orig := visibleLibraries
	visibleLibraries = func(account.ID) ([]string, error) { return get(), nil }
	t.Cleanup(func() { visibleLibraries = orig })
}

// recheckReturning replaces the credential re-read for one test.
func recheckReturning(t *testing.T, fn func(id string) (*credential.Credential, error)) {
	t.Helper()
	orig := recheckCredential
	recheckCredential = fn
	t.Cleanup(func() { recheckCredential = orig })
}

// stillGood is a recheck that finds the credential as it was.
func stillGood(t *testing.T, cred *credential.Credential) {
	t.Helper()
	recheckReturning(t, func(string) (*credential.Credential, error) { return cred, nil })
}

// A library that appears in the set after the socket was up rings, and its
// next commit is delivered.
//
// This is what makes the resync loop the mechanism and not a hook: a library
// created by another process, or granted by a path this server does not
// have yet, still appears -- one tick late rather than never.
func TestALibraryThatAppearsInTheSetRingsAndIsThenWatched(t *testing.T) {
	const created = "11111111-2222-3333-4444-555555555555"
	set := []string{testLibrary}
	visibleLibrariesFrom(t, func() []string { return set })
	stillGood(t, testCredential())

	conn, c := liveAccountClient(t)

	set = []string{testLibrary, created}
	c.resyncAccount()

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
	if n := subscriberCount(created); n != 1 {
		t.Fatalf("the new library has %d subscriber(s) after the resync, want 1", n)
	}

	NotifyLibraryUpdate(created, "0123456789abcdef0123456789abcdef01234567")
	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
}

// A library that leaves the set is unsubscribed, and the leaving rings.
//
// Deleted or unshared, the client's listing is stale either way, and the
// commit that will never come for it is not what tells it so.
func TestALibraryThatLeavesTheSetRingsAndIsNoLongerWatched(t *testing.T) {
	set := []string{testLibrary}
	visibleLibrariesFrom(t, func() []string { return set })
	stillGood(t, testCredential())

	conn, c := liveAccountClient(t)

	set = nil
	c.resyncAccount()

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
	if n := subscriberCount(testLibrary); n != 0 {
		t.Errorf("a library that left the set still has %d subscriber(s)", n)
	}
}

// An unchanged set rings nothing. The tick is a freshness check, and a client
// rung every five minutes for no reason would learn to ignore the ring.
func TestAnUnchangedSetDoesNotRing(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)
	stillGood(t, testCredential())

	conn, c := liveAccountClient(t)
	c.resyncAccount()

	expectNoFrame(t, conn, 200*time.Millisecond)
	if n := subscriberCount(testLibrary); n != 1 {
		t.Errorf("an unchanged resync left %d subscriber(s), want 1", n)
	}
}

// A credential revoked under a live account socket closes it on the next tick.
//
// Hygiene rather than the security boundary -- the ring carries nothing, and
// every fetch it provokes re-resolves the credential -- but a revoked device
// should not be told the account is active, and should not hold a socket for
// free.
func TestARevokedCredentialClosesTheAccountSocketOnResync(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)
	recheckReturning(t, func(string) (*credential.Credential, error) { return nil, credential.ErrInvalid })

	conn, c := liveAccountClient(t)
	c.resyncAccount()

	if !closedWithin(t, conn, 2*time.Second) {
		t.Fatal("the socket stayed open on a revoked credential")
	}
}

// A credential narrowed after the handshake closes the socket likewise.
//
// The account ring was granted to a credential that could answer for the
// whole account; one that no longer can must not keep ringing for it. The
// client reconnects, and its subscribe is answered by the credential it now
// holds.
func TestANarrowedCredentialClosesTheAccountSocketOnResync(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)
	recheckReturning(t, func(string) (*credential.Credential, error) {
		cred := testCredential()
		cred.Scope = credential.Scope{LibraryID: testLibrary}
		return cred, nil
	})

	conn, c := liveAccountClient(t)
	c.resyncAccount()

	if !closedWithin(t, conn, 2*time.Second) {
		t.Fatal("the socket stayed open on a credential narrowed since the handshake")
	}
}

// A store that cannot be reached is not a revocation.
//
// Dropping every account socket on a database hiccup would turn one slow
// query into a reconnect storm, each reconnect asking the same database. The
// tick logs and waits for the next one.
func TestAResyncThatCannotReadTheStoreKeepsTheSocket(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)
	recheckReturning(t, func(string) (*credential.Credential, error) {
		return nil, errors.New("database is locked")
	})

	conn, c := liveAccountClient(t)
	c.resyncAccount()

	if closedWithin(t, conn, 300*time.Millisecond) {
		t.Fatal("the socket was dropped because the store could not be read")
	}
	if n := subscriberCount(testLibrary); n != 1 {
		t.Errorf("a failed resync left %d subscriber(s), want 1", n)
	}
}

// A rename rings, with no commit and no head movement.
//
// This is the regression the account mode was asked for. A rename is one
// UPDATE on the catalog and mints no commit -- under E2EE the server could
// not write one -- so the head does not move, the per-library lane says
// nothing, and a client carried each library's name in its anchor purely to
// notice on its own.
func TestARenameRingsAnAccountSocketWithoutACommit(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)
	stillGood(t, testCredential())

	conn, _ := liveAccountClient(t)

	NotifyLibraryRenamed(testLibrary)

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
	if n := subscriberCount(testLibrary); n != 1 {
		t.Errorf("a rename left %d subscriber(s), want 1: the library is still there", n)
	}
}

// A library created rings its owner's sockets, and is watched from then on.
//
// Nothing was subscribed to the new library, so the per-library index cannot
// find the sockets to ring; this is the one change that needs the account
// key. The ring says the set moved, and the resync it provokes is what makes
// the next commit to the new library arrive.
func TestACreatedLibraryRingsItsOwnerAndIsThenWatched(t *testing.T) {
	const created = "11111111-2222-3333-4444-555555555555"
	set := []string{testLibrary}
	visibleLibrariesFrom(t, func() []string { return set })
	stillGood(t, testCredential())

	conn, _ := liveAccountClient(t)

	set = []string{testLibrary, created}
	NotifyAccountUpdate(testCredential().AccountID)

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
	waitForSubscribers(t, created, 1)

	NotifyLibraryUpdate(created, "0123456789abcdef0123456789abcdef01234567")
	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
}

// A library deleted rings everyone watching it, and they stop.
func TestADeletedLibraryRingsAndIsNoLongerWatched(t *testing.T) {
	set := []string{testLibrary}
	visibleLibrariesFrom(t, func() []string { return set })
	stillGood(t, testCredential())

	conn, _ := liveAccountClient(t)

	set = nil
	NotifyLibraryGone(testLibrary)

	awaitFrame(t, conn, EventTypeAccountUpdate, 2*time.Second)
	waitForSubscribers(t, testLibrary, 0)
}
