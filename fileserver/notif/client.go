package notif

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/dkam/silo/internal/observability"
)

const (
	writeWait  = 10 * time.Second
	pingPeriod = 30 * time.Second
	pongWait   = 90 * time.Second

	// checkTokenPeriod is how often each client sweeps its subscribed libraries
	// for expired JWTs. Subscribe JWTs are minted with a 72h lifetime in
	// sync_api.go, so hourly is frequent enough.
	checkTokenPeriod = 1 * time.Hour

	// wchBuffer is the outbound channel depth. Large enough to absorb a
	// burst of events without dropping; small enough that a stuck client
	// doesn't pin unbounded memory.
	wchBuffer = 32
)

type Client struct {
	ID uint64

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

	// lastPongUnix is read and written atomically.
	lastPongUnix atomic.Int64

	librariesMu sync.Mutex
	libraries   map[string]int64 // libraryID -> JWT exp (unix). nil after close.

	closeOnce sync.Once
	closeCh   chan struct{}
	wg        sync.WaitGroup
}

type subscribeFrame struct {
	Libraries []subscribeLibrary `json:"libraries"`
}

type subscribeLibrary struct {
	LibraryID string `json:"id"`
	Token     string `json:"jwt_token"`
}

// NewClient wires a freshly-upgraded WebSocket connection into the notif
// package and blocks until the connection ends. When it returns, the
// connection is closed and all bookkeeping has been cleaned up.
func NewClient(conn *websocket.Conn) {
	c := &Client{
		ID:        nextID(),
		conn:      conn,
		wch:       make(chan *Message, wchBuffer),
		libraries: make(map[string]int64),
		closeCh:   make(chan struct{}),
	}
	c.lastPongUnix.Store(time.Now().Unix())

	conn.SetPongHandler(func(string) error {
		c.lastPongUnix.Store(time.Now().Unix())
		return nil
	})

	c.wg.Add(4)
	go c.recover(c.readLoop)
	go c.recover(c.writeLoop)
	go c.recover(c.pingLoop)
	go c.recover(c.tokenExpiryLoop)
	c.wg.Wait()

	// Drain subscriptions. Setting libraries=nil blocks any late subscribe()
	// from a still-in-flight message to re-add state after close.
	c.librariesMu.Lock()
	ids := make([]string, 0, len(c.libraries))
	for id := range c.libraries {
		ids = append(ids, id)
	}
	c.libraries = nil
	c.librariesMu.Unlock()
	for _, id := range ids {
		removeSubscription(id, c)
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
	c.closeOnce.Do(func() { close(c.closeCh) })
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
			c.connMu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.conn.WriteJSON(msg)
			c.connMu.Unlock()
			if err != nil {
				log.Debugf("notif: client %d write error: %v", c.ID, err)
				return
			}
		case <-c.closeCh:
			return
		}
	}
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

func (c *Client) tokenExpiryLoop() {
	defer c.signalClose()
	ticker := time.NewTicker(checkTokenPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			now := time.Now().Unix()
			var expired []string
			c.librariesMu.Lock()
			for id, exp := range c.libraries {
				if exp < now {
					expired = append(expired, id)
				}
			}
			c.librariesMu.Unlock()
			for _, id := range expired {
				c.unsubscribe(id)
				c.sendJWTExpired(id)
			}
		case <-c.closeCh:
			return
		}
	}
}

func (c *Client) handleMessage(msg *Message) error {
	switch msg.Type {
	case "subscribe":
		var frame subscribeFrame
		if err := json.Unmarshal(msg.Content, &frame); err != nil {
			return fmt.Errorf("bad subscribe frame: %w", err)
		}
		for _, r := range frame.Libraries {
			user, exp, ok := parseNotifToken(r.Token, r.LibraryID)
			if !ok {
				c.sendJWTExpired(r.LibraryID)
				continue
			}
			c.subscribe(r.LibraryID, user, exp)
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

func (c *Client) subscribe(libraryID, user string, exp int64) {
	c.librariesMu.Lock()
	if c.libraries == nil {
		c.librariesMu.Unlock()
		return
	}
	c.libraries[libraryID] = exp
	if c.User == "" {
		c.User = user
	}
	c.librariesMu.Unlock()
	addSubscription(libraryID, c)
}

func (c *Client) unsubscribe(libraryID string) {
	c.librariesMu.Lock()
	delete(c.libraries, libraryID)
	c.librariesMu.Unlock()
	removeSubscription(libraryID, c)
}

func (c *Client) sendJWTExpired(libraryID string) {
	content, err := json.Marshal(map[string]string{"library_id": libraryID})
	if err != nil {
		return
	}
	msg := &Message{Type: EventTypeJWTExpired, Content: content}
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
