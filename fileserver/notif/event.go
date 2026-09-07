package notif

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"

	"github.com/dkam/silo/fileserver/account"
)

// Event type strings exchanged over the wire.
const (
	EventTypeLibraryUpdate = "library-update"

	// EventTypeSubscribeDenied answers a subscribe the server will not grant.
	//
	// It replaced jwt-expired, which could not carry this. That meant
	// "re-mint and try again", which was true of a stale token and false of a
	// library the caller may not reach -- a client told that about a
	// permission answer re-mints in a loop forever. And silence is worse than
	// either: the client believes it is subscribed, stops polling, and the
	// library appears to stop changing.
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

// ringFrame is the one frame every account-scoped socket gets. It carries
// nothing, and that is the point: there is nothing in it to have been
// authorized, so the socket needs no lease on any library it rings for. One
// frame serves every ring, because nothing writes to a queued message.
var ringFrame = &Message{Type: EventTypeAccountUpdate, Content: json.RawMessage("{}")}

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

	// Built on the first per-library target rather than up front: an account
	// socket is rung with the shared frame instead, so a library watched only
	// that way would marshal a payload nobody is sent -- on the commit path,
	// per commit.
	var update *Message
	for _, c := range targets {
		if c.accountScoped.Load() {
			c.ring()
			continue
		}
		if update == nil {
			content, err := json.Marshal(&LibraryUpdateEvent{LibraryID: libraryID, CommitID: commitID})
			if err != nil {
				log.Warnf("notif: failed to encode library-update event: %v", err)
				return
			}
			update = &Message{Type: EventTypeLibraryUpdate, Content: content}
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

// NotifyAccountUpdate tells every account-scoped socket of one account that
// its set moved, for the change that has no library to find them by: a
// library created, which nothing was subscribed to yet.
//
// Off the commit path, like NotifyLibraryGone: what it asks each socket for is
// a resync, which reads the database on the socket's own goroutine.
func NotifyAccountUpdate(acct account.ID) {
	for _, c := range snapshotAccountSockets(acct) {
		c.nudge()
	}
}

// NotifyLibraryRenamed rings every account-scoped socket watching a library
// whose name changed. The per-library index already knows who watches it, so
// neither the owner nor the grantees are looked up.
//
// It rings rather than nudging, which is the whole difference from
// NotifyLibraryGone. A nudge asks the socket to re-resolve its set and rings
// only if the set moved, or on being told the resync found nothing -- and a
// rename can never move a set, so every one of those queries is asked to
// produce an answer already known. That is four reads per socket, and a
// library shared with a hundred accounts has a socket per device of each.
//
// A per-library socket is not told. That lane's frame carries a commit id,
// and a rename mints no commit: the name is catalog data, which the account
// lane was built to cover.
func NotifyLibraryRenamed(libraryID string) {
	for _, c := range snapshotSubscribers(libraryID) {
		if c.accountScoped.Load() {
			c.ring()
		}
	}
}

// NotifyLibraryGone tells every account-scoped socket watching a library that
// it is no longer there. Unlike a rename this does move the set, so it asks
// for a resync: the socket has a subscription to drop, and dropping it is what
// the resync is for.
func NotifyLibraryGone(libraryID string) {
	for _, c := range snapshotSubscribers(libraryID) {
		if c.accountScoped.Load() {
			c.nudge()
		}
	}
}
