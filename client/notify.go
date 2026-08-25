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

	// Re-mint backoff, per library. A token the server rejects is usually one
	// that expired, and a fresh one fixes it; a permission that was revoked
	// will be rejected forever, which is what the ceiling is for.
	watchRemintMin = 250 * time.Millisecond
	watchRemintMax = 60 * time.Second

	// renewBefore is how far ahead of expiry a cached token is replaced,
	// rather than sent and rejected.
	renewBefore = 5 * time.Minute

	watchEventBuffer = 32
	watchFrameBuffer = 8
)

// The message types the protocol exchanges. The server's spelling of the two
// it sends is asserted against these in the tests, because a rename on that
// side would otherwise compile here and simply stop delivering events.
const (
	msgSubscribe     = "subscribe"
	msgLibraryUpdate = "library-update"
	msgJWTExpired    = "jwt-expired"
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
	Token     string `json:"jwt_token"`
}

// NotifyToken mints the library-scoped JWT the notification socket asks for.
// It is a separate token from the session's: the socket carries no
// Authorization header, and authorizes each subscription on its own.
func (c *APIClient) NotifyToken(libraryID string) (token string, expiresAt int64, err error) {
	var resp struct {
		Token     string `json:"jwt_token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	path := "/api/silo/v1/libraries/" + url.PathEscape(libraryID) + "/notify-token"
	if err := c.doRequest("POST", path, nil, &resp); err != nil {
		return "", 0, err
	}
	return resp.Token, resp.ExpiresAt, nil
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
	// library, or a token that has come due for another try.
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
	token   string
	expires int64 // unix seconds

	// retryAt holds a library out of the next subscribe frame after the server
	// refused its token, backoff is how long the hold grows to, and retry is
	// the timer that wakes the connection when the hold is up.
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
	// each one a JWT the server verifies under a lock its other clients are
	// waiting on.
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

// serverOffers asks whether this server has a notification endpoint at all,
// so a build without one is not dialled every 30 seconds for the life of the
// session. A server that will not answer gets the benefit of the doubt: an
// unreachable /server-info says nothing about /notification.
func (w *Watcher) serverOffers() bool {
	info, err := w.api.GetServerInfo()
	if err != nil {
		return true
	}
	return info.Has("notifications")
}

func (w *Watcher) dial() (*websocket.Conn, error) {
	endpoint, err := notifyEndpoint(w.api.BaseURL)
	if err != nil {
		return nil, err
	}
	// The session token, when there is one. The server accepts a socket
	// without it -- the endpoint is older than the header -- but a connection
	// that names an account may sit with nothing subscribed, which is exactly
	// what this watcher does between opening and the caller's first Subscribe.
	// Anonymous sockets have a deadline to subscribe by.
	var headers http.Header
	if token := w.api.getToken(); token != "" {
		headers = http.Header{"Authorization": []string{"Bearer " + token}}
	}

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
	// Subscribing has to mint tokens, which is an HTTP round trip. Off this
	// goroutine, because a loop parked on that request is a loop not reading
	// events -- and, since the request has no deadline of its own, a Close the
	// user waits on for as long as the server feels like taking.
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
	case msgJWTExpired:
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
func (w *Watcher) resubscribe(conn *websocket.Conn) ([]string, error) {
	var frame wireSubscribe
	var sent []string
	for _, id := range w.pending() {
		token, err := w.token(id)
		if err != nil {
			// The token endpoint is unreachable or says no. Back off on this
			// library alone; the rest of the frame still goes.
			w.deferRetry(id)
			continue
		}
		frame.Libraries = append(frame.Libraries, wireSubscribeLibrary{LibraryID: id, Token: token})
		sent = append(sent, id)
	}
	if len(frame.Libraries) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(watchWriteWait))
	if err := conn.WriteJSON(wireMessage{Type: msgSubscribe, Content: raw}); err != nil {
		return nil, err
	}
	return sent, nil
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

// token returns a usable notification token for libraryID, minting one if the
// cached token is missing or close enough to expiry to be refused.
func (w *Watcher) token(libraryID string) (string, error) {
	w.mu.Lock()
	if lib, ok := w.libs[libraryID]; ok && lib.token != "" && time.Until(time.Unix(lib.expires, 0)) > renewBefore {
		token := lib.token
		w.mu.Unlock()
		return token, nil
	}
	w.mu.Unlock()

	token, expires, err := w.api.NotifyToken(libraryID)
	if err != nil {
		return "", err
	}

	w.mu.Lock()
	if lib, ok := w.libs[libraryID]; ok {
		lib.token, lib.expires = token, expires
	}
	w.mu.Unlock()
	return token, nil
}

// deferRetry drops a library's token and holds it out of the subscribe frame
// for a growing interval, then wakes the connection to try again.
func (w *Watcher) deferRetry(libraryID string) {
	w.mu.Lock()
	lib, ok := w.libs[libraryID]
	if !ok {
		w.mu.Unlock()
		return
	}
	lib.token, lib.expires = "", 0
	if lib.backoff == 0 {
		lib.backoff = watchRemintMin
	} else {
		lib.backoff = min(lib.backoff*2, watchRemintMax)
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
