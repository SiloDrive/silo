package notif

import (
	"net/http"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/SiloDrive/silo/fileserver/middleware"
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
// A socket with no credential never reaches here: middleware.RequireSocketCredential
// refuses it, which is where every other route's 401 comes from too. It used
// to be refused in this function, back when the route was mounted optional so
// that an anonymous socket could prove itself later with a subscribe token.
//
// The account travels with the credential and separately. The account is who
// the socket belongs to; the credential is what that holder may reach, which
// is what a subscribe asks about. They are not the same question -- a
// credential narrowed to one library names a whole account.
func Handler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error response to the client.
		log.Debugf("notif: websocket upgrade failed: %v", err)
		return
	}
	NewClient(conn, middleware.GetAccount(r), middleware.GetCredential(r))
}
