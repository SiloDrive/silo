package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LibraryUpdate is one library-update event: the library's head has moved, and
// anything a client had listed from it may now be out of date.
type LibraryUpdate struct {
	LibraryID string `json:"library_id"`
	CommitID  string `json:"commit_id"`
}

const (
	// watchReadWait is how long the socket may be silent before it counts as
	// dead. The server pings every 30s, so this is three missed pings.
	watchReadWait  = 90 * time.Second
	watchWriteWait = 10 * time.Second

	// Dial backoff. A connection that lasted at least watchDialMin earns a
	// fresh start, so an occasional drop reconnects promptly and a server that
	// refuses the socket outright is not hammered.
	watchDialMin = 1 * time.Second
	watchDialMax = 30 * time.Second

	// Retry backoff, per library. A refused subscribe is usually a permission
	// that was withdrawn, and retrying will be refused forever -- which is
	// what the ceiling is for. It is retried at all because the other cause is
	// a share being re-granted, or a credential re-scoped, under a socket that
	// is still up.
	watchRetryMin = 250 * time.Millisecond
	watchRetryMax = 60 * time.Second

	watchEventBuffer = 32
	watchFrameBuffer = 8
)

// The message types the protocol exchanges. The server's spelling of the two
// it sends is asserted against these in the tests, because a rename on that
// side would otherwise compile here and simply stop delivering events.
const (
	msgSubscribe       = "subscribe"
	msgLibraryUpdate   = "library-update"
	msgSubscribeDenied = "subscribe-denied"
)

// wireMessage is the notification protocol's envelope, in both directions.
type wireMessage struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

type wireSubscribe struct {
	Libraries []wireSubscribeLibrary `json:"libraries"`
}

type wireSubscribeLibrary struct {
	LibraryID string `json:"id"`
}

// Watcher keeps a notification socket open and turns library-update events
// into a channel a caller can select on.
//
// It owns the reconnecting: a caller says which libraries it cares about and
// reads Events, and the socket coming and going underneath is not its problem.
type Watcher struct {
	api    *APIClient
	events chan LibraryUpdate
	// wake asks the live connection to re-assert subscriptions — a new
	// library, or one whose retry hold has come due.
	wake chan struct{}

	// cancel stops everything; done is what the loops select on. A context
	// rather than a bare channel because the dial takes one, and a Close that
	// had to wait out a 15s handshake would be a Close the TUI feels.
	cancel context.CancelFunc
	done   <-chan struct{}
	wg     sync.WaitGroup

	// ctx is held only to hand to the dialer.
	ctx context.Context

	mu   sync.Mutex
	libs map[string]*watchedLibrary
}

type watchedLibrary struct {
	// retryAt holds a library out of the next subscribe frame after the server
	// refused it, backoff is how long the hold grows to, and retry is the
	// timer that wakes the connection when the hold is up.
	retryAt time.Time
	backoff time.Duration
	retry   *time.Timer
}

// Watch starts watching. Nothing is subscribed until Subscribe is called, and
// the caller must Close the watcher when it is done with it.
func (c *APIClient) Watch() *Watcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		api:    c,
		events: make(chan LibraryUpdate, watchEventBuffer),
		wake:   make(chan struct{}, 1),
		cancel: cancel,
		done:   ctx.Done(),
		libs:   make(map[string]*watchedLibrary),
	}
	w.ctx = ctx
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run()
	}()
	return w
}

// Events yields every library-update for a subscribed library. It is closed
// once Close has stopped the watcher.
func (w *Watcher) Events() <-chan LibraryUpdate { return w.events }

// Subscribe adds a library to the watched set. It is idempotent, and it does
// not block on the network: the subscription is asserted on the current
// connection, or on the next one to come up.
func (w *Watcher) Subscribe(libraryID string) {
	if libraryID == "" {
		return
	}
	w.mu.Lock()
	_, known := w.libs[libraryID]
	if !known {
		w.libs[libraryID] = &watchedLibrary{}
	}
	w.mu.Unlock()
	// Only a new library is worth waking the connection for. Re-entering a
	// library already watched would otherwise re-send every subscription held,
	// each one a permission the server re-checks under a lock its other
	// clients are waiting on.
	if !known {
		w.nudge()
	}
}

// Close stops the watcher and waits for it to finish. It is safe to call more
// than once, and safe to call on a watcher whose socket never came up.
func (w *Watcher) Close() {
	w.cancel()
	w.mu.Lock()
	for _, lib := range w.libs {
		if lib.retry != nil {
			lib.retry.Stop()
		}
	}
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *Watcher) nudge() {
	select {
	case w.wake <- struct{}{}:
	default: // a wake is already pending, and one is as good as two
	}
}

// sleep waits for d, or returns false if the watcher is closed first.
func (w *Watcher) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-w.done:
		return false
	}
}

func (w *Watcher) run() {
	defer close(w.events)

	if !w.serverOffers() {
		return
	}

	backoff := watchDialMin
	served := false
	for {
		conn, err := w.dial()
		if err == nil {
			start := time.Now()
			// Every connection but the first is a reconnection, and a
			// reconnection has a gap behind it.
			w.serve(conn, served)
			served = true
			if time.Since(start) >= watchDialMin {
				backoff = watchDialMin
			}
		}
		// Unconditional, including after a connection that worked: a server
		// that accepts the socket and drops it immediately must not become a
		// reconnect loop with no pause in it.
		if !w.sleep(backoff) {
			return
		}
		backoff = min(backoff*2, watchDialMax)
	}
}

// serverOffers asks whether this server has a notification endpoint this
// watcher can use, so a build without one is not dialled every 30 seconds for
// the life of the session. A server that will not answer gets the benefit of
// the doubt: an unreachable /server-info says nothing about /notification.
//
// Both names are required, and the second one is why this is not simply a
// reachability check. This watcher subscribes with no jwt_token, and a server
// old enough to want one answers that frame with jwt-expired -- which means
// "re-mint and try again", so a client that believed it would re-mint forever
// against an endpoint that has nothing to give it. notifications-credential is
// the server saying it authorizes a subscribe from the handshake instead.
func (w *Watcher) serverOffers() bool {
	info, err := w.api.GetServerInfo()
	if err != nil {
		return true
	}
	return info.Has("notifications") && info.Has("notifications-credential")
}

func (w *Watcher) dial() (*websocket.Conn, error) {
	endpoint, err := notifyEndpoint(w.api.BaseURL)
	if err != nil {
		return nil, err
	}
	// The credential, always. The server refuses the handshake without it --
	// every subscribe is answered from it, so a socket carrying none could be
	// subscribed to nothing. Sending an empty one is left to fail as a 401
	// rather than short-circuited here: a Watch before login is a caller bug,
	// and the retry loop reports it the same way it reports any other refused
	// dial.
	headers := http.Header{"Authorization": []string{"Bearer " + w.api.getToken()}}

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(w.ctx, endpoint, headers)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return conn, err
}

// notifyEndpoint turns a server's base URL into its notification socket's.
func notifyEndpoint(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("bad server URL %q: %v", baseURL, err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("cannot open a notification socket on scheme %q", u.Scheme)
	}
	// The endpoint hangs off whatever prefix the server is mounted under.
	u.Path = strings.TrimSuffix(u.Path, "/") + "/notification"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// serve runs one connection until it fails or the watcher closes. resync says
// this connection follows a gap, and the libraries it re-subscribes should be
// announced as possibly-changed once they are back in place.
func (w *Watcher) serve(conn *websocket.Conn, resync bool) {
	frames := make(chan wireMessage, watchFrameBuffer)
	stop := make(chan struct{})
	var reader sync.WaitGroup

	reader.Add(1)
	go func() {
		defer reader.Done()
		defer close(frames)
		w.readLoop(conn, frames, stop)
	}()
	// Off this goroutine so that a loop asserting subscriptions is not a loop
	// that has stopped reading events. It used to be the stronger claim that
	// subscribing meant an HTTP round trip to mint a token per library; a
	// subscribe frame is now a socket write, and the separation is kept
	// because the wake loop belongs to it either way.
	go w.assertLoop(conn, stop, resync)

	defer func() {
		// In this order: stop releases a reader blocked on a full frames
		// buffer, Close releases one blocked on the socket.
		close(stop)
		_ = conn.Close()
		reader.Wait()
	}()

	for {
		select {
		case <-w.done:
			return
		case msg, ok := <-frames:
			if !ok {
				return
			}
			if !w.handle(msg) {
				return
			}
		}
	}
}

// assertLoop keeps this connection's subscriptions up to date: everything
// watched when it opens, and whatever a wake adds or releases from a retry
// hold after that. A wake that arrives with no connection up is not lost --
// the next connection asserts the whole set as it opens.
func (w *Watcher) assertLoop(conn *websocket.Conn, stop <-chan struct{}, resync bool) {
	for {
		asserted, err := w.resubscribe(conn)
		if err != nil {
			return
		}
		if resync {
			resync = false
			w.announceResync(asserted)
		}
		select {
		case <-w.wake:
		case <-stop:
			return
		case <-w.done:
			return
		}
	}
}

func (w *Watcher) readLoop(conn *websocket.Conn, frames chan<- wireMessage, stop <-chan struct{}) {
	_ = conn.SetReadDeadline(time.Now().Add(watchReadWait))
	conn.SetPingHandler(func(data string) error {
		// The server's ping is the only proof the connection is still there,
		// so it is what the read deadline is measured from.
		_ = conn.SetReadDeadline(time.Now().Add(watchReadWait))
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(watchWriteWait))
		if err == websocket.ErrCloseSent {
			return nil
		}
		return err
	})

	for {
		var msg wireMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		select {
		case frames <- msg:
		case <-stop:
			return
		case <-w.done:
			return
		}
	}
}

// handle acts on one inbound frame, and reports whether the connection should
// stay up.
func (w *Watcher) handle(msg wireMessage) bool {
	switch msg.Type {
	case msgLibraryUpdate:
		var ev LibraryUpdate
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			return true // a frame we cannot read is not a connection we must drop
		}
		select {
		case w.events <- ev:
		case <-w.done:
			return false
		}
	case msgSubscribeDenied:
		var ev struct {
			LibraryID string `json:"library_id"`
		}
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			return true
		}
		w.deferRetry(ev.LibraryID)
	}
	return true
}

// resubscribe asserts every watched library that is not in a retry hold, and
// returns the ones it asserted. The server treats a repeated subscribe as the
// same subscription, so re-sending one already in place costs nothing.
//
// It asserts all of them or none. There was a per-library failure here when
// each entry needed a token minted for it; with the token gone the frame is
// built from ids alone, so the only way to send some is to fail to send any.
func (w *Watcher) resubscribe(conn *websocket.Conn) ([]string, error) {
	ids := w.pending()
	if len(ids) == 0 {
		return nil, nil
	}
	frame := wireSubscribe{Libraries: make([]wireSubscribeLibrary, 0, len(ids))}
	for _, id := range ids {
		frame.Libraries = append(frame.Libraries, wireSubscribeLibrary{LibraryID: id})
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(watchWriteWait))
	if err := conn.WriteJSON(wireMessage{Type: msgSubscribe, Content: raw}); err != nil {
		return nil, err
	}
	return ids, nil
}

// announceResync tells the caller that these libraries may have moved while
// the socket was down. Nothing replays the events missed during a gap, so the
// gap itself is the news, and the caller reloads rather than trusting a
// listing taken before it. The commit is left empty: what it should be is
// exactly what the watcher does not know.
func (w *Watcher) announceResync(libraryIDs []string) {
	for _, id := range libraryIDs {
		select {
		case w.events <- LibraryUpdate{LibraryID: id}:
		case <-w.done:
			return
		}
	}
}

// pending is the watched libraries whose retry hold, if any, has passed.
func (w *Watcher) pending() []string {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.libs))
	for id, lib := range w.libs {
		if lib.retryAt.After(now) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// deferRetry holds a library out of the subscribe frame for a growing
// interval, then wakes the connection to try again.
func (w *Watcher) deferRetry(libraryID string) {
	w.mu.Lock()
	lib, ok := w.libs[libraryID]
	if !ok {
		w.mu.Unlock()
		return
	}
	if lib.backoff == 0 {
		lib.backoff = watchRetryMin
	} else {
		lib.backoff = min(lib.backoff*2, watchRetryMax)
	}
	lib.retryAt = time.Now().Add(lib.backoff)
	// One timer per library, not one per rejection: a library the server keeps
	// refusing would otherwise stack a live timer for every refusal.
	if lib.retry != nil {
		lib.retry.Stop()
	}
	lib.retry = time.AfterFunc(lib.backoff, w.nudge)
	w.mu.Unlock()
}
