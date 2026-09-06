package notif

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// subscriberCount reports how many clients are subscribed to libraryID.
func subscriberCount(libraryID string) int {
	return len(snapshotSubscribers(libraryID))
}

// waitForSubscribers blocks until libraryID has n subscribers, or fails.
//
// A subscribe frame is handled on the server's read loop, so the write that
// sent it returning says nothing about the subscription existing yet.
func waitForSubscribers(t *testing.T, libraryID string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if subscriberCount(libraryID) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("library %s has %d subscriber(s), want %d", libraryID, subscriberCount(libraryID), n)
}

// sendSubscribe writes a subscribe frame for one library.
//
// With a Token it is the token lane; without one it is the credential lane,
// and the absence is the whole signal: it says "authorize this from whatever
// opened the socket", which is what every other route already does.
func sendSubscribe(t *testing.T, conn *websocket.Conn, lib subscribeLibrary) {
	t.Helper()
	content, err := json.Marshal(subscribeFrame{Libraries: []subscribeLibrary{lib}})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(&Message{Type: "subscribe", Content: content}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// awaitFrame reads until a frame of the given type arrives, or the deadline.
func awaitFrame(t *testing.T, conn *websocket.Conn, typ string, d time.Duration) *Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if msg.Type == typ {
			return &msg
		}
	}
}

// awaitUpdate reads until an update for libraryID arrives, and returns it.
func awaitUpdate(t *testing.T, conn *websocket.Conn, libraryID string) LibraryUpdateEvent {
	t.Helper()
	for {
		msg := awaitFrame(t, conn, EventTypeLibraryUpdate, 10*time.Second)
		var ev LibraryUpdateEvent
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			t.Fatalf("bad event content: %v", err)
		}
		if ev.LibraryID == libraryID {
			return ev
		}
	}
}

// expectQueued fails unless the next frame queued on c is of the given type.
func expectQueued(t *testing.T, c *Client, typ string) {
	t.Helper()
	select {
	case msg := <-c.wch:
		if msg.Type != typ {
			t.Errorf("got %q, want %q", msg.Type, typ)
		}
	default:
		t.Errorf("no %s frame was queued", typ)
	}
}
