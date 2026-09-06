package notif

import (
	"testing"
	"time"
)

// subscriberCount reports how many clients are subscribed to libraryID.
func subscriberCount(libraryID string) int {
	subMu.RLock()
	defer subMu.RUnlock()
	subs, ok := subscriptions[libraryID]
	if !ok {
		return 0
	}
	return len(subs.clients)
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
