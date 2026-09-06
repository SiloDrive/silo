package notif

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
)

// visibleLibrariesReturning replaces the visible-set query for one test.
//
// Owned and granted are two tables in two packages, and the socket's tests
// own neither. What is under test is what the socket does with the answer.
func visibleLibrariesReturning(t *testing.T, ids ...string) {
	t.Helper()
	orig := visibleLibraries
	visibleLibraries = func(account.ID) ([]string, error) { return ids, nil }
	t.Cleanup(func() { visibleLibraries = orig })
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

// An account subscribe with no credential is refused.
//
// The account's set is resolved from the credential that opened the socket,
// and an anonymous socket has nothing to resolve it from. The token lane
// cannot stand in: a token names one library, and this frame names none.
func TestAnAccountSubscribeWithNoCredentialIsRefused(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)

	conn := dialSocket(t, nil)
	subscribeToAccount(t, conn)

	awaitFrame(t, conn, EventTypeSubscribeDenied, 2*time.Second)
	if n := subscriberCount(testLibrary); n != 0 {
		t.Errorf("an anonymous account subscribe registered %d subscriber(s)", n)
	}
}

// A narrowed credential is refused the account's set.
//
// It cannot answer for the set: a scope is a ceiling below the account, and
// the frames this lane sends are about libraries the scope excludes. The
// scoped ring, when it lands, is what turns this refusal into a subscription
// to the one library the scope names -- until then the refusal is the whole
// answer, and the token lane still works for such a credential.
func TestAnAccountSubscribeFromANarrowedCredentialIsRefused(t *testing.T) {
	visibleLibrariesReturning(t, testLibrary)

	cred := testCredential()
	cred.Scope = credential.Scope{LibraryID: testLibrary}
	conn := dialWithCredential(t, cred)
	subscribeToAccount(t, conn)

	awaitFrame(t, conn, EventTypeSubscribeDenied, 2*time.Second)
	if n := subscriberCount(testLibrary); n != 0 {
		t.Errorf("a narrowed account subscribe registered %d subscriber(s)", n)
	}
}

// A commit to a library the account can see reaches an account-scoped socket
// that never named that library.
//
// This is the mode's whole point: the client subscribes once and is told about
// every library in its set, including the ones it has not heard of yet.
func TestACommitToAVisibleLibraryReachesAnAccountScopedSocket(t *testing.T) {
	const commitID = "0123456789abcdef0123456789abcdef01234567"
	visibleLibrariesReturning(t, testLibrary)

	conn := dialWithCredential(t, testCredential())
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	NotifyLibraryUpdate(testLibrary, commitID)
	awaitUpdate(t, conn, testLibrary)
}

// A commit to a library the account cannot see reaches it not at all.
//
// Every ring is gated on the same set the listing already answers with. A
// socket that heard about a library outside that set would be a listing that
// leaks, one library id at a time.
func TestACommitToAnInvisibleLibraryDoesNotReachAnAccountScopedSocket(t *testing.T) {
	const other = "11111111-2222-3333-4444-555555555555"
	visibleLibrariesReturning(t, testLibrary)

	conn := dialWithCredential(t, testCredential())
	subscribeToAccount(t, conn)
	waitForSubscribers(t, testLibrary, 1)

	if n := subscriberCount(other); n != 0 {
		t.Fatalf("an account subscribe registered %d subscriber(s) on a library outside its set", n)
	}
	NotifyLibraryUpdate(other, "0123456789abcdef0123456789abcdef01234567")
	expectNoFrame(t, conn, 200*time.Millisecond)
}
