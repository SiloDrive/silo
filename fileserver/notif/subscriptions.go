// Package notif implements the in-process notification server.
//
// It exposes a WebSocket endpoint a client connects to so it receives push
// events when a library's head commit changes, instead of polling on a ~30s
// interval.
//
// It handles "library-update" events and nothing else, because nothing else has a
// producer: file locking, a sharing API and comments are the event types the
// design it descends from also carried, and Silo ships none of them.
package notif

import (
	"sync"
	"sync/atomic"

	"github.com/SiloDrive/silo/fileserver/account"
)

var (
	// subMu protects both indexes below and the client maps inside them. Held
	// in write mode for subscribe/unsubscribe, read mode for a fanout's
	// snapshot.
	subMu sync.RWMutex

	// subscriptions is the clients watching each library.
	subscriptions index[string]

	// accountSockets is the clients subscribed to each account's whole set.
	// It exists for the one change the per-library index cannot find a socket
	// by: a library created, which nothing was subscribed to yet. Every other
	// change reaches the socket through the library it is about.
	accountSockets index[account.ID]

	nextClientID uint64
)

// index is clients grouped by something they watch -- a library, or an
// account. The two differ only in the key, and every operation on them is the
// same three lines about the same lock, so they are one type rather than two
// families of function that must be kept in step.
//
// Every method assumes subMu is held by the caller, in the mode the method
// needs. The lock is package-wide rather than per-index because a subscribe
// touches one and a teardown touches both, and one lock is what makes that
// ordering a non-question.
type index[K comparable] map[K]map[uint64]*Client

func (ix index[K]) add(key K, c *Client) {
	clients, ok := ix[key]
	if !ok {
		clients = make(map[uint64]*Client)
		ix[key] = clients
	}
	clients[c.ID] = c
}

// remove drops a client, and drops the key with the last client under it so
// the index does not grow unbounded across the lifetime of the process.
func (ix index[K]) remove(key K, c *Client) {
	clients, ok := ix[key]
	if !ok {
		return
	}
	delete(clients, c.ID)
	if len(clients) == 0 {
		delete(ix, key)
	}
}

// snapshot copies out the clients under a key, or nil if there are none. A
// copy because a fanout writes to each client after the lock is released.
func (ix index[K]) snapshot(key K) []*Client {
	clients := ix[key]
	if len(clients) == 0 {
		return nil
	}
	out := make([]*Client, 0, len(clients))
	for _, c := range clients {
		out = append(out, c)
	}
	return out
}

// Init resets package state. Call once at server startup.
func Init() {
	subMu.Lock()
	subscriptions = make(index[string])
	accountSockets = make(index[account.ID])
	subMu.Unlock()
}

func nextID() uint64 {
	return atomic.AddUint64(&nextClientID, 1)
}

func addSubscription(libraryID string, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	subscriptions.add(libraryID, c)
}

func removeSubscription(libraryID string, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	subscriptions.remove(libraryID, c)
}

func snapshotSubscribers(libraryID string) []*Client {
	subMu.RLock()
	defer subMu.RUnlock()
	return subscriptions.snapshot(libraryID)
}

func addAccountSocket(acct account.ID, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	accountSockets.add(acct, c)
}

func removeAccountSocket(acct account.ID, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	accountSockets.remove(acct, c)
}

func snapshotAccountSockets(acct account.ID) []*Client {
	subMu.RLock()
	defer subMu.RUnlock()
	return accountSockets.snapshot(acct)
}
