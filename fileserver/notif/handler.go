package notif

import (
	"net/http"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/dkam/silo/fileserver/middleware"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 1024,
	// The notification endpoint is opened by sync clients, which don't have
	// a meaningful Origin header. Accept any origin, matching upstream
	// notification-server behavior.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Handler upgrades a request into a notification socket.
//
// The account is whatever authenticated the request, and may be nil: the
// endpoint predates the header, so a client that sends none is still served.
// What it does not get is the right to sit there -- see provisionalGrace.
func Handler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error response to the client.
		log.Debugf("notif: websocket upgrade failed: %v", err)
		return
	}
	NewClient(conn, acct)
}
