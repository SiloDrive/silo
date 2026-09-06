package notif

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
)

// Event type strings exchanged over the wire.
const (
	EventTypeLibraryUpdate = "library-update"
	EventTypeJWTExpired    = "jwt-expired"

	// EventTypeSubscribeDenied answers a subscribe the server will not grant
	// on the credential lane.
	//
	// jwt-expired cannot carry this. It means "re-mint and try again", which
	// is true of a stale token and false of a library the caller may not
	// reach -- a client told that about a permission answer re-mints in a
	// loop forever. And silence is worse than either: the client believes it
	// is subscribed, stops polling, and the library appears to stop changing.
	//
	// It names the library and nothing else. Why the answer was no is in the
	// log, where the operator is the audience, for the reason
	// middleware.credentialRefused gives.
	EventTypeSubscribeDenied = "subscribe-denied"

	// EventTypeAccountUpdate is the bare ring an account-scoped socket gets
	// where a per-library socket gets library-update: something in the set
	// moved, and GET /libraries says what.
	EventTypeAccountUpdate = "account-update"
)

// Message is the wire format exchanged with clients. Both inbound
// (subscribe/unsubscribe) and outbound (the event types above) frames use it.
type Message struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

// LibraryUpdateEvent is the payload for a library-update message.
type LibraryUpdateEvent struct {
	LibraryID string `json:"library_id"`
	CommitID  string `json:"commit_id"`
}

// accountRing is the frame an account-scoped socket gets. It carries nothing,
// and that is the point: there is nothing in it to have been authorized, so
// the socket needs no lease on any library it rings for.
func accountRing() *Message {
	return &Message{Type: EventTypeAccountUpdate, Content: json.RawMessage("{}")}
}

// NotifyLibraryUpdate fans a library-update event out to every client currently
// subscribed to libraryID. Delivery is best-effort and non-blocking: if a
// client's write channel is full, the message is dropped for that client
// rather than back-pressuring the caller (which is the commit-write hot path).
//
// The frame is chosen per client, not per event. A per-library socket gets
// the library and the commit it subscribed to hear about; an account socket
// gets a bare ring, because it was never checked against this library in
// particular and the frame must carry nothing that check would have covered.
// One set of subscribers, one branch at the point of writing.
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
	update := &Message{Type: EventTypeLibraryUpdate, Content: content}

	for _, c := range targets {
		if c.accountScoped.Load() {
			c.ring()
			continue
		}
		select {
		case c.wch <- update:
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
