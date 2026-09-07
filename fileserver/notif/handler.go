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
// A request with no credential is refused here rather than upgraded.
//
// It used to be served. The endpoint predated the Authorization header, and a
// subscribe frame carried a library-scoped JWT that authorized itself, so an
// anonymous socket could still prove something -- it just had a deadline to do
// it by, provisionalGrace, before being reaped as an idle stranger. With that
// lane gone every subscribe is answered from the credential this handshake
// carried, so a socket without one can subscribe to nothing, ever. Refusing it
// at the handshake says that once, with a status code, instead of accepting a
// connection whose every frame will be denied and which a timer must then
// clean up.
//
// The account travels with the credential, and separately. The account is who
// the socket belongs to; the credential is what that holder may reach, which
// is what a subscribe asks about. They are not the same question -- a
// credential narrowed to one library names a whole account.
func Handler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	cred := middleware.GetCredential(r)
	if cred == nil {
		log.Debugf("notif: refused a socket that presented no credential")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error response to the client.
		log.Debugf("notif: websocket upgrade failed: %v", err)
		return
	}
	NewClient(conn, acct, cred)
}
