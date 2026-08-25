package notif

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
)

// Event type strings exchanged over the wire.
const (
	EventTypeLibraryUpdate = "library-update"
	EventTypeJWTExpired    = "jwt-expired"
)

// Message is the wire format exchanged with clients. Both inbound
// (subscribe/unsubscribe) and outbound (library-update, jwt-expired) frames use
// it.
type Message struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// LibraryUpdateEvent is the payload for a library-update message.
type LibraryUpdateEvent struct {
	LibraryID string `json:"library_id"`
	CommitID  string `json:"commit_id"`
}

// NotifyLibraryUpdate fans a library-update event out to every client currently
// subscribed to libraryID. Delivery is best-effort and non-blocking: if a
// client's write channel is full, the message is dropped for that client
// rather than back-pressuring the caller (which is the commit-write hot path).
func NotifyLibraryUpdate(libraryID, commitID string) {
	targets := snapshotSubscribers(libraryID)
	if len(targets) == 0 {
		return
	}

	content, err := json.Marshal(&LibraryUpdateEvent{LibraryID: libraryID, CommitID: commitID})
	if err != nil {
		log.Warnf("notif: failed to encode library-update event: %v", err)
		return
	}
	msg := &Message{Type: EventTypeLibraryUpdate, Content: content}

	for _, c := range targets {
		select {
		case c.wch <- msg:
		default:
			// Not dropped -- deferred. The send stays non-blocking because
			// this runs on the commit path and one stuck socket must not hold
			// up a write, but a client that is merely behind is owed the news
			// rather than denied it, and it gets it as soon as its queue moves.
			log.Debugf("notif: deferring library-update for slow client %d", c.ID)
			c.noteMissed(libraryID, commitID)
		}
	}
}
