package notif

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkam/silo/fileserver/utils"
)

// liveClient dials a real notification socket and returns both ends of it: the
// test's connection and the server's Client for the same connection.
//
// A real socket rather than a fake, because what is being tested is what
// arrives at the far end, and the write path is the thing under test.
func liveClient(t *testing.T, libraryID string) (*websocket.Conn, *Client) {
	t.Helper()
	Init()

	srv := httptest.NewServer(http.HandlerFunc(Handler))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	tok, err := utils.GenNotifJWTToken(libraryID, "watcher@example.com", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	content, err := json.Marshal(subscribeFrame{
		Libraries: []subscribeLibrary{{LibraryID: libraryID, Token: tok}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(&Message{Type: "subscribe", Content: content}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Subscribing is asynchronous: the frame is read and acted on by the
	// client's own goroutine, so the server may not have recorded it yet.
	for i := 0; i < 200; i++ {
		if subs := snapshotSubscribers(libraryID); len(subs) == 1 {
			return conn, subs[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the server never recorded the subscription")
	return nil, nil
}

// stall blocks a client's writer and fills its outbound buffer, returning the
// unlock.
//
// This is the state a client on a slow link reaches by itself, and holding the
// mutex the writer takes before every write is how a test reaches it on
// demand rather than by luck. The writer has already taken one message off the
// channel and parked on the lock, so filling is done by non-blocking send
// until it refuses -- which is exact regardless of how far the writer got
// first.
func stall(t *testing.T, c *Client) func() {
	t.Helper()
	c.connMu.Lock()

	filler, err := json.Marshal(&LibraryUpdateEvent{LibraryID: "filler", CommitID: "0"})
	if err != nil {
		t.Fatal(err)
	}
fillLoop:
	for {
		select {
		case c.wch <- &Message{Type: EventTypeLibraryUpdate, Content: filler}:
		default:
			break fillLoop
		}
	}
	return c.connMu.Unlock
}

// awaitUpdate reads until an update for libraryID arrives, and returns it.
func awaitUpdate(t *testing.T, conn *websocket.Conn, libraryID string) LibraryUpdateEvent {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("no update for %s ever arrived: %v", libraryID, err)
		}
		if msg.Type != EventTypeLibraryUpdate {
			continue
		}
		var ev LibraryUpdateEvent
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			t.Fatalf("bad event content: %v", err)
		}
		if ev.LibraryID == libraryID {
			return ev
		}
	}
}

// An event dropped because a client was behind is delivered once it catches up.
//
// The fanout is non-blocking on purpose -- it runs on the commit hot path, and
// one stuck socket must not hold up a write. But a client whose buffer was full
// simply lost the event: it stayed subscribed, heard nothing more, and went on
// showing a listing from before the change until some later commit happened to
// arrive. Nothing replays it and no client can ask for it, so a TUI left open
// on a busy library could sit indefinitely on a stale directory with a healthy
// connection and no error anywhere.
//
// The socket coming back after a gap already announces itself (see the Watcher's
// resubscribe). This is the same gap without the disconnection, which is the
// harder one to notice and the one nothing covered.
func TestAnEventDroppedForASlowClientArrivesWhenItCatchesUp(t *testing.T) {
	const libraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	conn, c := liveClient(t, libraryID)
	resume := stall(t, c)

	// Dropped: there is no room for it.
	NotifyLibraryUpdate(libraryID, commitID)

	resume()

	ev := awaitUpdate(t, conn, libraryID)
	if ev.CommitID != commitID {
		t.Errorf("caught up with commit %q, want %q", ev.CommitID, commitID)
	}
}

// Several drops for one library collapse into one delivery, carrying the last.
//
// The debt is what the client still needs to know, not a log of what it missed.
// Every dropped update for a library is superseded by the next one -- they all
// say "this library moved" and only the newest says where to -- so replaying
// them in order would be a burst of messages whose first ones are already
// wrong by the time they are sent.
func TestSeveralDropsForOneLibraryCollapseIntoTheNewest(t *testing.T) {
	const libraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const newest = "9999999999999999999999999999999999999999"

	conn, c := liveClient(t, libraryID)
	resume := stall(t, c)

	NotifyLibraryUpdate(libraryID, "1111111111111111111111111111111111111111")
	NotifyLibraryUpdate(libraryID, "2222222222222222222222222222222222222222")
	NotifyLibraryUpdate(libraryID, newest)

	resume()

	ev := awaitUpdate(t, conn, libraryID)
	if ev.CommitID != newest {
		t.Errorf("first delivery carried commit %q, want the newest %q", ev.CommitID, newest)
	}

	// And nothing further for this library: the superseded ones are not queued
	// behind it.
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			return // the read deadline: nothing more came, which is the point
		}
		if msg.Type != EventTypeLibraryUpdate {
			continue
		}
		var ev LibraryUpdateEvent
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			t.Fatalf("bad event content: %v", err)
		}
		if ev.LibraryID == libraryID {
			t.Errorf("a second update for %s arrived carrying %q; the drops should have collapsed",
				libraryID, ev.CommitID)
		}
	}
}
