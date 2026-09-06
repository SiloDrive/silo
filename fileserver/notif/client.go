package notif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/dkam/silo/internal/observability"
)

const (
	writeWait  = 10 * time.Second
	pingPeriod = 30 * time.Second
	pongWait   = 90 * time.Second

	// sweepPeriod is how often each client re-examines what it is subscribed
	// to: expired JWTs on the token lane, withdrawn access on the credential
	// lane. Subscribe JWTs are minted with a 72h lifetime, so hourly is
	// frequent enough for those, and it is the same order as the time a
	// client would take to notice through polling.
	sweepPeriod = 1 * time.Hour

	// resyncPeriod is how often an account socket re-resolves the credential
	// behind it and the set it rings for. A freshness number, not an
	// authorization one: the ring carries nothing, and every fetch it
	// provokes re-resolves the credential on its own. Five minutes matches
	// the sweep the desktop client already runs against the listing.
	resyncPeriod = 5 * time.Minute

	// wchBuffer is the outbound channel depth. Large enough to absorb a
	// burst of events without dropping; small enough that a stuck client
	// doesn't pin unbounded memory.
	wchBuffer = 32
)

// provisionalGrace is how long a connection that arrived without a credential
// may hold a socket without subscribing to anything.
//
// A connection with no subscriptions receives nothing, ever -- the socket
// carries library-update and nothing else -- so an anonymous one that never
// subscribes is pure cost. Subscribing proves something: the frame carries a
// library-scoped JWT the server verifies. So this is not a new authentication
// requirement, it is a deadline on the one that was already there.
//
// Generous on purpose. It has to cover a client that connects, discovers its
// notification token has expired, mints another and tries again, on a bad
// link. The thing being prevented is a socket held indefinitely for free, and
// thirty seconds does that just as well as one would.
//
// Atomic, and read through graceWindow. Production only ever reads it, but
// tests shorten it while sockets accepted by a still-running test server are
// reading -- and a hijacked connection is not a request the server waits for,
// so there is no ordering between the two to rely on.
var provisionalGrace atomic.Int64

const defaultProvisionalGrace = 30 * time.Second

func init() { provisionalGrace.Store(int64(defaultProvisionalGrace)) }

func graceWindow() time.Duration { return time.Duration(provisionalGrace.Load()) }

type Client struct {
	ID uint64

	// account is the account named by a credential at the handshake, or nil
	// for a connection that offered none. A named connection may sit idle; an
	// anonymous one has provisionalGrace to subscribe. It is also what a
	// per-account connection limit will count, when there is one.
	account *account.Account

	// cred is the credential that authenticated the handshake, or nil for a
	// connection that offered none. It is what authorizes a subscribe on the
	// credential lane -- see authorize.go -- and it is held for the life of
	// the socket because the sweep asks the same question again.
	cred *credential.Credential

	// User is the authenticated username, set on the first
	// successful subscribe from the JWT claims. It is unused today (only
	// library-update events flow, and those fan out to every subscriber of a
	// library), but will be needed when per-user events like
	// folder-perm-changed are added — the upstream notification server
	// filters fanout by this field. See notification-server/event.go for
	// the upstream pattern.
	User string

	conn   *websocket.Conn
	connMu sync.Mutex // serializes writes to conn
	wch    chan *Message

	// missed is the debt this client is owed: library id -> the commit id of
	// the most recent update that could not be handed to wch. resyncCh wakes
	// the writer to pay it, and is buffered by one because the writer takes
	// the whole set at once -- a second nudge for a set already pending is
	// nothing to remember.
	missedMu sync.Mutex
	missed   map[string]string
	resyncCh chan struct{}

	// resyncNow asks the account socket to re-resolve its set before the
	// next tick: a hook has said something moved. Buffered by one for the
	// reason resyncCh is.
	resyncNow chan struct{}

	// lastPongUnix is read and written atomically.
	lastPongUnix atomic.Int64

	librariesMu sync.Mutex
	libraries   map[string]subscription // libraryID -> subscription. nil after close.

	// accountScoped says the client asked for its account's whole set, and
	// holds a laneAccount subscription for each library in it. It is what
	// the fanout branches on -- an account socket is rung, not told -- and
	// atomic because the fanout runs on the commit path and takes no lock of
	// this client's.
	accountScoped atomic.Bool

	// ringOwed is the account socket's whole debt: a ring could not be
	// handed to wch, and one is due when the queue moves. One bit rather
	// than the per-library map above, because there is nothing in a ring to
	// collapse -- every one says the same thing.
	ringOwed atomic.Bool

	// graceTimer closes the connection if provisionalGrace passes with nothing
	// subscribed. nil when the handshake carried a credential.
	graceTimer *time.Timer

	closeOnce sync.Once
	closeCh   chan struct{}
	wg        sync.WaitGroup
}

// subscription is one library this client watches, and which lane authorized
// it.
//
// The lane is a field rather than an inference from exp, even though the token
// lane is the only one that carries an expiry. The sweep treats the lanes
// completely differently -- one is re-checked against the database, one
// against the clock -- and a rule that reads "exp == 0 means ask the
// authorizer" is one refactor away from silently reclassifying every
// subscription on the wrong lane.
type subscription struct {
	lane lane

	// exp is the JWT's expiry in unix seconds on the token lane, and 0 on the
	// others, where nothing in the subscription expires on its own.
	exp int64
}

// lane is what authorized a subscription, and so what can end it.
type lane int

const (
	// laneToken is a library-scoped JWT presented in the subscribe frame. It
	// ends on the token's expiry.
	laneToken lane = iota

	// laneCredential is the socket's credential, asked about one library the
	// frame named. It ends when the sweep finds the credential no longer
	// reaches the library.
	laneCredential

	// laneAccount is the socket's credential, asked for every library the
	// account can see. One frame registers the whole set, and the set moves
	// underneath it; the resync loop is what keeps it true, and nothing here
	// re-checks it until that loop lands.
	laneAccount
)

type subscribeFrame struct {
	Libraries []subscribeLibrary `json:"libraries"`

	// Account asks for every library the socket's account can see, resolved
	// from the credential that opened the socket. See subscribeAccount.
	Account bool `json:"account,omitempty"`
}

// subscribeLibrary is one entry of a subscribe or unsubscribe frame.
//
// An absent Token is the credential lane, and the absence is the whole signal:
// it says "authorize this from whatever opened the socket", which is what every
// other route in Silo already does. A client that sends one is not asking for
// anything a token would not have got it -- it is declining to fetch a token
// first.
type subscribeLibrary struct {
	LibraryID string `json:"id"`
	Token     string `json:"jwt_token,omitempty"`
}

// NewClient wires a freshly-upgraded WebSocket connection into the notif
// package and blocks until the connection ends. When it returns, the
// connection is closed and all bookkeeping has been cleaned up.
func NewClient(conn *websocket.Conn, acct *account.Account, cred *credential.Credential) {
	c := &Client{
		ID:        nextID(),
		account:   acct,
		cred:      cred,
		conn:      conn,
		wch:       make(chan *Message, wchBuffer),
		missed:    make(map[string]string),
		resyncCh:  make(chan struct{}, 1),
		resyncNow: make(chan struct{}, 1),
		libraries: make(map[string]subscription),
		closeCh:   make(chan struct{}),
	}
	c.lastPongUnix.Store(time.Now().Unix())

	if acct != nil {
		c.User = acct.Email
	}

	conn.SetPongHandler(func(string) error {
		c.lastPongUnix.Store(time.Now().Unix())
		return nil
	})

	// An anonymous connection is on the clock from the moment it is accepted.
	// Started before the loops so that a client which connects and then says
	// nothing at all is still on it.
	if acct == nil {
		c.graceTimer = time.AfterFunc(graceWindow(), c.dropIfUnproven)
	}

	c.wg.Add(4)
	go c.recover(c.readLoop)
	go c.recover(c.writeLoop)
	go c.recover(c.pingLoop)
	go c.recover(c.sweepLoop)
	c.wg.Wait()

	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}

	// Drain subscriptions. Setting libraries=nil blocks any late subscribe()
	// from a still-in-flight message to re-add state after close.
	c.librariesMu.Lock()
	ids := make([]string, 0, len(c.libraries))
	for id := range c.libraries {
		ids = append(ids, id)
	}
	c.libraries = nil
	c.librariesMu.Unlock()

	// nil rather than empty, so a fanout still holding a pointer to this
	// client notes nothing rather than growing a debt nobody will ever pay.
	c.missedMu.Lock()
	c.missed = nil
	c.missedMu.Unlock()
	for _, id := range ids {
		removeSubscription(id, c)
	}
	if c.accountScoped.Load() {
		removeAccountSocket(c.account.ID, c)
	}
	_ = conn.Close()
}

func (c *Client) recover(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			observability.Panic(context.Background(), fmt.Sprintf("notif client %d", c.ID), r)
		}
		c.wg.Done()
	}()
	fn()
}

func (c *Client) signalClose() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		// Closing the connection is what ends readLoop. Nothing sets a read
		// deadline, so a peer that stops talking but leaves the socket open
		// parks that goroutine in ReadJSON forever -- and because NewClient
		// waits on all four, the connection and its bookkeeping went with it.
		// Every other loop returns on closeCh; this one only returns when the
		// read fails, so the read has to be made to fail.
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

// dropIfUnproven closes a connection that arrived without a credential and has
// not subscribed to anything since.
//
// Checked once rather than polled: the deadline is from the handshake, and a
// connection that subscribes stops the timer rather than resetting it. There
// is no second chance to schedule -- once something is subscribed the
// connection has proved itself for good, and if it later unsubscribes
// everything it is a client that is still talking, not an idle stranger.
func (c *Client) dropIfUnproven() {
	c.librariesMu.Lock()
	subscribed := len(c.libraries)
	c.librariesMu.Unlock()
	if subscribed > 0 {
		return
	}
	log.Debugf("notif: closing client %d: no credential at the handshake and nothing subscribed within %v",
		c.ID, graceWindow())
	c.signalClose()
}

func (c *Client) readLoop() {
	defer c.signalClose()
	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			log.Debugf("notif: client %d read error: %v", c.ID, err)
			return
		}
		if err := c.handleMessage(&msg); err != nil {
			log.Debugf("notif: client %d handle error: %v", c.ID, err)
			return
		}
	}
}

func (c *Client) writeLoop() {
	defer c.signalClose()
	for {
		select {
		case msg := <-c.wch:
			if err := c.writeMessage(msg); err != nil {
				log.Debugf("notif: client %d write error: %v", c.ID, err)
				return
			}
		case <-c.resyncCh:
			// Woken by the drop itself rather than by the next successful
			// write. Flushing after a write would be the obvious place -- a
			// slot has just come free -- but it races: a flush that runs
			// between a failed send and the note it leaves behind finds an
			// empty set, and the debt then waits for an unrelated event that
			// may never come.
			if err := c.writeMissed(); err != nil {
				log.Debugf("notif: client %d resync write error: %v", c.ID, err)
				return
			}
			if c.ringOwed.Swap(false) {
				if err := c.writeMessage(accountRing()); err != nil {
					log.Debugf("notif: client %d ring write error: %v", c.ID, err)
					return
				}
			}
		case <-c.closeCh:
			return
		}
	}
}

// writeMessage sends one frame. The lock is what makes the three writers --
// this loop, the resync flush and the pinger -- one writer as far as the
// connection is concerned.
func (c *Client) writeMessage(msg *Message) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteJSON(msg)
}

// noteMissed records that an update could not be delivered, and wakes the
// writer to deliver it when it can.
//
// A later drop for the same library overwrites an earlier one, because they do
// not accumulate into anything. Every one of them says "this library moved",
// and only the newest says where to -- replaying the older ones would send a
// burst whose leading messages are already wrong.
func (c *Client) noteMissed(libraryID, commitID string) {
	c.missedMu.Lock()
	if c.missed == nil {
		// Closed. Nothing to deliver it over.
		c.missedMu.Unlock()
		return
	}
	c.missed[libraryID] = commitID
	c.missedMu.Unlock()

	select {
	case c.resyncCh <- struct{}{}:
	default:
		// Already nudged, and the flush takes whatever the set holds when it
		// runs -- including what was just added.
	}
}

// noteRingOwed records that a ring could not be delivered, and wakes the
// writer to deliver one when it can. Later drops change nothing: the bit is
// already set, and one ring pays for all of them.
func (c *Client) noteRingOwed() {
	c.ringOwed.Store(true)
	select {
	case c.resyncCh <- struct{}{}:
	default:
	}
}

// takeMissed removes and returns the whole debt.
//
// Taken rather than read, so that a drop arriving during the write that
// follows re-notes and re-nudges instead of being lost in the handover.
func (c *Client) takeMissed() map[string]string {
	c.missedMu.Lock()
	defer c.missedMu.Unlock()
	if len(c.missed) == 0 {
		return nil
	}
	out := c.missed
	c.missed = make(map[string]string)
	return out
}

// writeMissed delivers what a slow client missed.
//
// Written straight to the connection rather than queued, because the queue
// being full is the whole reason there is anything to deliver -- a resync that
// could itself be dropped would leave the client exactly where it started.
//
// A failure part way through abandons the rest, and does not put them back.
// The connection is ending, and the socket that replaces it announces every
// library it re-subscribes as possibly-changed, which covers strictly more
// than what is being abandoned here.
func (c *Client) writeMissed() error {
	for libraryID, commitID := range c.takeMissed() {
		content, err := json.Marshal(&LibraryUpdateEvent{LibraryID: libraryID, CommitID: commitID})
		if err != nil {
			log.Warnf("notif: failed to encode a missed library-update: %v", err)
			continue
		}
		if err := c.writeMessage(&Message{Type: EventTypeLibraryUpdate, Content: content}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) pingLoop() {
	defer c.signalClose()
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			last := c.lastPongUnix.Load()
			if time.Since(time.Unix(last, 0)) > pongWait {
				log.Debugf("notif: client %d stale (no pong for %v)", c.ID, pongWait)
				return
			}
			c.connMu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.connMu.Unlock()
			if err != nil {
				log.Debugf("notif: client %d ping error: %v", c.ID, err)
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

func (c *Client) sweepLoop() {
	defer c.signalClose()
	sweep := time.NewTicker(sweepPeriod)
	defer sweep.Stop()
	resync := time.NewTicker(resyncPeriod)
	defer resync.Stop()
	for {
		select {
		case <-sweep.C:
			c.sweepSubscriptions()
		case <-resync.C:
			if c.accountScoped.Load() {
				c.resyncAccount()
			}
		case <-c.resyncNow:
			// A hook said the set moved, or that something in it did. The
			// resync rings if it finds a difference; a rename is a change it
			// cannot see, so a resync that found nothing rings anyway.
			if c.accountScoped.Load() && !c.resyncAccount() {
				c.ring()
			}
		case <-c.closeCh:
			return
		}
	}
}

// sweepSubscriptions drops what this client should no longer be receiving.
//
// The two lanes fail in different ways and are told about it differently. A
// token lapses on a clock both sides can read, and jwt-expired asks for a
// fresh one. A credential-authorized subscription has no clock: what ends it
// is a share being withdrawn, which the client cannot see coming and cannot
// fix by re-minting, so it gets subscribe-denied instead.
//
// The credential half is the price of the lane. A token bounded a withdrawn
// share at 72 hours by expiry alone, whether or not anyone noticed; a device
// credential need never expire, so without re-asking, a subscription would
// outlive the access that authorized it for the life of the process -- and the
// frames it goes on receiving carry a commit id.
func (c *Client) sweepSubscriptions() {
	now := time.Now().Unix()

	// Decided under the lock, acted on after it. The credential half asks the
	// database, and the lock is the one the read loop takes for every
	// subscribe -- so the ids are collected here and the question is asked
	// once the lock is gone.
	var byCredential []string
	dropped := map[string]string{} // library id -> the frame that says why
	c.librariesMu.Lock()
	for id, sub := range c.libraries {
		switch sub.lane {
		case laneCredential:
			byCredential = append(byCredential, id)
		case laneToken:
			if sub.exp < now {
				dropped[id] = EventTypeJWTExpired
			}
		}
	}
	c.librariesMu.Unlock()

	for _, id := range byCredential {
		if !authorized(c.cred, id) {
			log.Infof("notif: client %d loses library %s: the credential that authorized it no longer reaches it", c.ID, id)
			dropped[id] = EventTypeSubscribeDenied
		}
	}
	for id, frame := range dropped {
		c.unsubscribe(id)
		c.sendLibraryFrame(frame, id)
	}
}

func (c *Client) handleMessage(msg *Message) error {
	switch msg.Type {
	case "subscribe":
		var frame subscribeFrame
		if err := json.Unmarshal(msg.Content, &frame); err != nil {
			return fmt.Errorf("bad subscribe frame: %w", err)
		}
		if frame.Account {
			c.subscribeAccount()
		}
		for _, r := range frame.Libraries {
			// No token means the credential lane. The two are never mixed for
			// one library: a frame either presents proof or asks the server to
			// use the proof it already has.
			if r.Token == "" {
				if !authorized(c.cred, r.LibraryID) {
					log.Debugf("notif: client %d refused a credential subscribe to %q", c.ID, r.LibraryID)
					c.sendLibraryFrame(EventTypeSubscribeDenied, r.LibraryID)
					continue
				}
				c.subscribe(r.LibraryID, "", subscription{lane: laneCredential})
				continue
			}
			user, exp, ok := parseNotifToken(r.Token, r.LibraryID)
			if !ok {
				c.sendLibraryFrame(EventTypeJWTExpired, r.LibraryID)
				continue
			}
			c.subscribe(r.LibraryID, user, subscription{lane: laneToken, exp: exp})
		}
		return nil
	case "unsubscribe":
		var frame subscribeFrame
		if err := json.Unmarshal(msg.Content, &frame); err != nil {
			return fmt.Errorf("bad unsubscribe frame: %w", err)
		}
		for _, r := range frame.Libraries {
			c.unsubscribe(r.LibraryID)
		}
		return nil
	default:
		log.Debugf("notif: client %d: ignoring unknown message type %q", c.ID, msg.Type)
		return nil
	}
}

func (c *Client) subscribe(libraryID, user string, sub subscription) {
	c.librariesMu.Lock()
	if c.libraries == nil {
		c.librariesMu.Unlock()
		return
	}
	c.libraries[libraryID] = sub
	if c.User == "" {
		c.User = user
	}
	c.librariesMu.Unlock()
	addSubscription(libraryID, c)

	// Proved. Either the token in the frame was verified to get here or the
	// credential behind the socket was, which is the thing the deadline was
	// waiting for.
	if c.graceTimer != nil {
		c.graceTimer.Stop()
	}
}

// subscribeAccount registers the client for every library its account can see.
//
// The set is resolved here, once, and registered in the same per-library index
// every other subscription uses. NotifyLibraryUpdate does not learn about
// accounts: it runs inside the commit write, and a reverse index consulted per
// event would put a query on that path to serve a mode one client uses. The
// cost is that the registration is a snapshot of a set that moves, which is
// the resync loop's problem to solve and not this function's.
//
// Two refusals, both answered with subscribe-denied naming no library. No
// credential means nothing to resolve the set from; the token lane cannot
// stand in, because a token names one library and this frame names none. A
// narrowed credential cannot answer for the set either: its scope is a ceiling
// below the account, and this lane's frames are about libraries the scope
// excludes. That second refusal is what the scoped ring will turn into a
// subscription to the one library the scope names; until it lands, a narrowed
// credential keeps the token lane, which works for it exactly as before.
func (c *Client) subscribeAccount() {
	if c.cred == nil || c.account == nil {
		log.Debugf("notif: client %d refused an account subscribe: no credential on the socket", c.ID)
		c.sendAccountDenied()
		return
	}
	if c.cred.Scope.LibraryID != "" {
		log.Debugf("notif: client %d refused an account subscribe: credential %s is scoped to %q", c.ID, c.cred.ID, c.cred.Scope)
		c.sendAccountDenied()
		return
	}
	ids, err := visibleLibraries(c.account.ID)
	if err != nil {
		log.Errorf("notif: client %d: resolving the libraries account %s can see: %v", c.ID, c.account.ID, err)
		c.sendAccountDenied()
		return
	}

	c.accountScoped.Store(true)
	addAccountSocket(c.account.ID, c)
	for _, id := range ids {
		c.subscribe(id, "", subscription{lane: laneAccount})
	}
}

// resyncAccount is one tick of the account socket's loop: is the credential
// still good, and is the set still the set.
//
// This loop is the mechanism, and the hooks that ring on create, delete and
// rename are only latency. It is the one thing that covers a change made by
// a process that is not this one -- an operator's command, a repair script, a
// second server on the same database -- and it delivers every kind of change
// with no producer anywhere else.
//
// The credential half is hygiene rather than the security boundary. The ring
// carries nothing, and every fetch it provokes re-resolves the credential on
// its own; what this stops is a revoked device being told the account is
// active, and holding a socket for free. Gone, expired, disabled and narrowed
// all close the socket, because in each case the credential that was granted
// the account's set can no longer answer for it. A store that cannot be read
// is none of those: dropping every account socket on a slow query would turn
// it into a reconnect storm asking the same database, so that tick is logged
// and the next one asks again.
//
// It reports whether the client has been told anything -- rung, or closed --
// so a caller acting on a hook can ring for the change the resync could not
// see.
func (c *Client) resyncAccount() bool {
	cred, err := recheckCredential(c.cred.ID)
	switch {
	case errors.Is(err, credential.ErrInvalid), errors.Is(err, credential.ErrExpired), errors.Is(err, credential.ErrInactive):
		log.Infof("notif: closing client %d: credential %s no longer resolves: %v", c.ID, c.cred.ID, err)
		c.signalClose()
		return true
	case err != nil:
		log.Warnf("notif: client %d: re-reading credential %s: %v", c.ID, c.cred.ID, err)
		return false
	case cred.Scope.LibraryID != "":
		log.Infof("notif: closing client %d: credential %s was narrowed to %q since it subscribed to the account", c.ID, c.cred.ID, cred.Scope)
		c.signalClose()
		return true
	}

	ids, err := visibleLibraries(c.account.ID)
	if err != nil {
		log.Warnf("notif: client %d: resolving the libraries account %s can see: %v", c.ID, c.account.ID, err)
		return false
	}
	visible := make(map[string]bool, len(ids))
	for _, id := range ids {
		visible[id] = true
	}

	var appeared, left []string
	c.librariesMu.Lock()
	for id, sub := range c.libraries {
		if sub.lane == laneAccount && !visible[id] {
			left = append(left, id)
		}
	}
	for _, id := range ids {
		if _, ok := c.libraries[id]; !ok {
			appeared = append(appeared, id)
		}
	}
	c.librariesMu.Unlock()

	for _, id := range appeared {
		c.subscribe(id, "", subscription{lane: laneAccount})
	}
	for _, id := range left {
		c.unsubscribe(id)
	}
	if len(appeared)+len(left) == 0 {
		return false
	}
	c.ring()
	return true
}

// nudge asks for a resync before the next tick. Non-blocking, and a second
// nudge for a resync already pending is nothing to remember.
func (c *Client) nudge() {
	select {
	case c.resyncNow <- struct{}{}:
	default:
	}
}

// ring hands the account socket its frame, or notes that one is owed.
func (c *Client) ring() {
	select {
	case c.wch <- accountRing():
	default:
		c.noteRingOwed()
	}
}

// sendAccountDenied answers an account subscribe the server will not grant.
// The same frame as a per-library refusal, naming the mode instead of a
// library, so a client reads one type for "no" on either lane.
func (c *Client) sendAccountDenied() {
	c.queueFrame(EventTypeSubscribeDenied, map[string]bool{"account": true})
}

func (c *Client) unsubscribe(libraryID string) {
	c.librariesMu.Lock()
	delete(c.libraries, libraryID)
	c.librariesMu.Unlock()
	removeSubscription(libraryID, c)

	// A library the client no longer follows is not owed a resync. Leaving the
	// debt would send an update for something it has said it is not watching,
	// which is at best ignored and at worst a reload of a library it just left.
	c.missedMu.Lock()
	delete(c.missed, libraryID)
	c.missedMu.Unlock()
}

// sendLibraryFrame queues one of the frames whose whole payload is a library
// id: jwt-expired, or subscribe-denied.
func (c *Client) sendLibraryFrame(typ, libraryID string) {
	c.queueFrame(typ, map[string]string{"library_id": libraryID})
}

// queueFrame queues a frame that answers the client rather than reporting a
// commit. Dropped rather than deferred when the queue is full: unlike an
// update, there is nothing here that becomes wrong by arriving late, and the
// sweep or the client that produced it asks again.
func (c *Client) queueFrame(typ string, content any) {
	raw, err := json.Marshal(content)
	if err != nil {
		return
	}
	msg := &Message{Type: typ, Content: raw}
	select {
	case c.wch <- msg:
	default:
	}
}

// parseNotifToken validates a library-scoped notification JWT. On success it
// returns the claimed username, expiry (unix seconds), and true.
func parseNotifToken(tokenString, libraryID string) (string, int64, bool) {
	// An empty libraryID would otherwise match the empty LibraryID claim of a
	// session token, which is signed with the same key.
	if tokenString == "" || libraryID == "" {
		return "", 0, false
	}
	claims := &utils.MyClaims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims,
		func(*jwt.Token) (any, error) {
			return []byte(option.JWTPrivateKey), nil
		},
		// Pin the algorithm rather than accepting whatever the token's header
		// asks for, and require the notification audience so a session token
		// cannot be replayed here.
		jwt.WithValidMethods([]string{utils.SigningAlg}),
		jwt.WithAudience(utils.AudNotif),
	)
	if err != nil || !tok.Valid {
		return "", 0, false
	}
	if claims.LibraryID != libraryID {
		return "", 0, false
	}
	exp, err := claims.GetExpirationTime()
	if err != nil || exp == nil || exp.Before(time.Now()) {
		return "", 0, false
	}
	return claims.UserName, exp.Unix(), true
}
