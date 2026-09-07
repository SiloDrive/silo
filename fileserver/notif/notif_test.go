package notif

import (
	"encoding/json"
	"testing"
	"time"
)

// fakeClient builds a minimal Client suitable for driving the fanout path
// without a real WebSocket connection.
func fakeClient() *Client {
	return &Client{
		ID:        nextID(),
		wch:       make(chan *Message, wchBuffer),
		libraries: make(map[string]lane),
	}
}

func TestNotifyLibraryUpdateFanout(t *testing.T) {
	Init()

	const libraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	c1 := fakeClient()
	c2 := fakeClient()
	other := fakeClient()

	addSubscription(libraryID, c1)
	addSubscription(libraryID, c2)
	addSubscription("11111111-2222-3333-4444-555555555555", other)

	NotifyLibraryUpdate(libraryID, commitID)

	for i, c := range []*Client{c1, c2} {
		select {
		case msg := <-c.wch:
			if msg.Type != "library-update" {
				t.Errorf("client %d: got type %q, want library-update", i, msg.Type)
			}
			var ev LibraryUpdateEvent
			if err := json.Unmarshal(msg.Content, &ev); err != nil {
				t.Fatalf("client %d: bad content: %v", i, err)
			}
			if ev.LibraryID != libraryID || ev.CommitID != commitID {
				t.Errorf("client %d: got %+v, want library=%s commit=%s", i, ev, libraryID, commitID)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: timed out waiting for library-update", i)
		}
	}

	// The unrelated subscriber must not have received anything.
	select {
	case msg := <-other.wch:
		t.Errorf("unrelated client got unexpected message: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyLibraryUpdateNoSubscribers(t *testing.T) {
	Init()
	// Should not panic and should return quickly when nothing is subscribed.
	NotifyLibraryUpdate("nobody-home", "deadbeef")
}

const testLibrary = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestRemoveSubscriptionStopsDelivery(t *testing.T) {
	Init()

	const libraryID = "ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
	c := fakeClient()
	addSubscription(libraryID, c)
	removeSubscription(libraryID, c)

	NotifyLibraryUpdate(libraryID, "0000000000000000000000000000000000000000")

	select {
	case msg := <-c.wch:
		t.Errorf("unsubscribed client got message: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}
