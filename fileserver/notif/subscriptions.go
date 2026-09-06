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

	"github.com/dkam/silo/fileserver/account"
)

var (
	// subMu protects subscriptions and (transitively) each subscribers.clients
	// map. Held in write mode for subscribe/unsubscribe, read mode for
	// NotifyLibraryUpdate's snapshot.
	subMu         sync.RWMutex
	subscriptions map[string]*subscribers

	// accountSockets is the clients subscribed to each account's whole set,
	// under subMu with the rest. It exists for the one change the per-library
	// index cannot find a socket by: a library created, which nothing was
	// subscribed to yet. Every other change reaches the socket through the
	// library it is about.
	accountSockets map[account.ID]map[uint64]*Client

	nextClientID uint64
)

type subscribers struct {
	clients map[uint64]*Client
}

// Init resets package state. Call once at server startup.
func Init() {
	subMu.Lock()
	subscriptions = make(map[string]*subscribers)
	accountSockets = make(map[account.ID]map[uint64]*Client)
	subMu.Unlock()
}

func nextID() uint64 {
	return atomic.AddUint64(&nextClientID, 1)
}

func addSubscription(libraryID string, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	subs, ok := subscriptions[libraryID]
	if !ok {
		subs = &subscribers{clients: make(map[uint64]*Client)}
		subscriptions[libraryID] = subs
	}
	subs.clients[c.ID] = c
}

// removeSubscription removes a client's subscription for libraryID. When the
// last subscriber leaves a library, the outer map entry is deleted so
// subscriptions doesn't grow unbounded across the lifetime of the process.
func removeSubscription(libraryID string, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	subs, ok := subscriptions[libraryID]
	if !ok {
		return
	}
	delete(subs.clients, c.ID)
	if len(subs.clients) == 0 {
		delete(subscriptions, libraryID)
	}
}

// snapshotSubscribers returns a copy of the clients currently subscribed to
// libraryID, or nil if no one is subscribed.
func snapshotSubscribers(libraryID string) []*Client {
	subMu.RLock()
	defer subMu.RUnlock()
	subs, ok := subscriptions[libraryID]
	if !ok || len(subs.clients) == 0 {
		return nil
	}
	out := make([]*Client, 0, len(subs.clients))
	for _, c := range subs.clients {
		out = append(out, c)
	}
	return out
}

func addAccountSocket(acct account.ID, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	socks, ok := accountSockets[acct]
	if !ok {
		socks = make(map[uint64]*Client)
		accountSockets[acct] = socks
	}
	socks[c.ID] = c
}

func removeAccountSocket(acct account.ID, c *Client) {
	subMu.Lock()
	defer subMu.Unlock()
	socks, ok := accountSockets[acct]
	if !ok {
		return
	}
	delete(socks, c.ID)
	if len(socks) == 0 {
		delete(accountSockets, acct)
	}
}

// snapshotAccountSockets returns a copy of the clients subscribed to an
// account's set, or nil.
func snapshotAccountSockets(acct account.ID) []*Client {
	subMu.RLock()
	defer subMu.RUnlock()
	socks := accountSockets[acct]
	if len(socks) == 0 {
		return nil
	}
	out := make([]*Client, 0, len(socks))
	for _, c := range socks {
		out = append(out, c)
	}
	return out
}
