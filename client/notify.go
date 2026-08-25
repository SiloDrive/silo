package client

import (
	"encoding/json"
	"fmt"
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
	done chan struct{}

	closeOnce sync.Once
	wg        sync.WaitGroup

	mu   sync.Mutex
	libs map[string]*watchedLibrary
}

type watchedLibrary struct {
	token   string
	expires int64 // unix seconds

	// retryAt holds a library out of the next subscribe frame after the server
	// refused its token, and backoff is how long the hold grows to.
	retryAt time.Time
	backoff time.Duration
}

// Watch starts watching. Nothing is subscribed until Subscribe is called, and
// the caller must Close the watcher when it is done with it.
func (c *APIClient) Watch() *Watcher {
	w := &Watcher{
		api:    c,
		events: make(chan LibraryUpdate, watchEventBuffer),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		libs:   make(map[string]*watchedLibrary),
	}
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
	if _, ok := w.libs[libraryID]; !ok {
		w.libs[libraryID] = &watchedLibrary{}
	}
	w.mu.Unlock()
	w.nudge()
}

// Close stops the watcher and waits for it to finish. It is safe to call more
// than once, and safe to call on a watcher whose socket never came up.
func (w *Watcher) Close() {
	w.closeOnce.Do(func() { close(w.done) })
	w.wg.Wait()
}

func (w *Watcher) nudge() {
	select {
	case w.wake <- struct{}{}:
	default: // a wake is already pending, and one is as good as two
	}
}

func (w *Watcher) stopping() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
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

	backoff := watchDialMin
	for {
		if w.stopping() {
			return
		}
		conn, err := w.dial()
		if err == nil {
			start := time.Now()
			w.serve(conn)
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

func (w *Watcher) dial() (*websocket.Conn, error) {
	endpoint, err := notifyEndpoint(w.api.BaseURL)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.Dial(endpoint, nil)
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

// serve runs one connection until it fails or the watcher closes.
func (w *Watcher) serve(conn *websocket.Conn) {
	frames := make(chan wireMessage, watchFrameBuffer)
	stop := make(chan struct{})
	var reader sync.WaitGroup

	reader.Add(1)
	go func() {
		defer reader.Done()
		defer close(frames)
		w.readLoop(conn, frames, stop)
	}()
	defer func() {
		// In this order: stop releases a reader blocked on a full frames
		// buffer, Close releases one blocked on the socket.
		close(stop)
		_ = conn.Close()
		reader.Wait()
	}()

	if err := w.resubscribe(conn); err != nil {
		return
	}
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
		case <-w.wake:
			if err := w.resubscribe(conn); err != nil {
				return
			}
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
	case "library-update":
		var ev LibraryUpdate
		if err := json.Unmarshal(msg.Content, &ev); err != nil {
			return true // a frame we cannot read is not a connection we must drop
		}
		w.subscriptionWorked(ev.LibraryID)
		select {
		case w.events <- ev:
		case <-w.done:
			return false
		}
	case "jwt-expired":
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

// resubscribe asserts every watched library that is not in a retry hold. The
// server treats a repeated subscribe as the same subscription, so re-sending
// one already in place costs nothing.
func (w *Watcher) resubscribe(conn *websocket.Conn) error {
	var frame wireSubscribe
	for _, id := range w.pending() {
		token, err := w.token(id)
		if err != nil {
			// The token endpoint is unreachable or says no. Back off on this
			// library alone; the rest of the frame still goes.
			w.deferRetry(id)
			continue
		}
		frame.Libraries = append(frame.Libraries, wireSubscribeLibrary{LibraryID: id, Token: token})
	}
	if len(frame.Libraries) == 0 {
		return nil
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(watchWriteWait))
	return conn.WriteJSON(wireMessage{Type: "subscribe", Content: raw})
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
	lib, ok := w.libs[libraryID]
	if !ok {
		w.mu.Unlock()
		return "", fmt.Errorf("library %s is not watched", libraryID)
	}
	if lib.token != "" && time.Until(time.Unix(lib.expires, 0)) > renewBefore {
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
	// Still watched? Close or a future Unsubscribe may have landed meanwhile.
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
	delay := lib.backoff
	lib.retryAt = time.Now().Add(delay)
	w.mu.Unlock()

	time.AfterFunc(delay, w.nudge)
}

// subscriptionWorked clears a library's retry hold: an event arrived, so the
// subscription behind it is live.
func (w *Watcher) subscriptionWorked(libraryID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lib, ok := w.libs[libraryID]; ok {
		lib.retryAt, lib.backoff = time.Time{}, 0
	}
}
