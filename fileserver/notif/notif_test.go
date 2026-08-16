package notif

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	jwt "github.com/golang-jwt/jwt/v5"
)

func init() {
	option.JWTPrivateKey = "test-secret-key-for-unit-tests"
}

// fakeClient builds a minimal Client suitable for driving the fanout path
// without a real WebSocket connection.
func fakeClient() *Client {
	return &Client{
		ID:    nextID(),
		wch:   make(chan *Message, wchBuffer),
		repos: make(map[string]int64),
	}
}

func TestNotifyRepoUpdateFanout(t *testing.T) {
	Init()

	const repoID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	c1 := fakeClient()
	c2 := fakeClient()
	other := fakeClient()

	addSubscription(repoID, c1)
	addSubscription(repoID, c2)
	addSubscription("11111111-2222-3333-4444-555555555555", other)

	NotifyRepoUpdate(repoID, commitID)

	for i, c := range []*Client{c1, c2} {
		select {
		case msg := <-c.wch:
			if msg.Type != "repo-update" {
				t.Errorf("client %d: got type %q, want repo-update", i, msg.Type)
			}
			var ev RepoUpdateEvent
			if err := json.Unmarshal(msg.Content, &ev); err != nil {
				t.Fatalf("client %d: bad content: %v", i, err)
			}
			if ev.RepoID != repoID || ev.CommitID != commitID {
				t.Errorf("client %d: got %+v, want repo=%s commit=%s", i, ev, repoID, commitID)
			}
		case <-time.After(time.Second):
			t.Errorf("client %d: timed out waiting for repo-update", i)
		}
	}

	// The unrelated subscriber must not have received anything.
	select {
	case msg := <-other.wch:
		t.Errorf("unrelated client got unexpected message: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyRepoUpdateNoSubscribers(t *testing.T) {
	Init()
	// Should not panic and should return quickly when nothing is subscribed.
	NotifyRepoUpdate("nobody-home", "deadbeef")
}

const testRepo = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestParseNotifTokenAcceptsGenuineToken(t *testing.T) {
	exp := time.Now().Add(72 * time.Hour).Unix()
	tok, err := utils.GenNotifJWTToken(testRepo, "alice@example.com", exp)
	if err != nil {
		t.Fatalf("failed to generate notif token: %v", err)
	}

	user, gotExp, ok := parseNotifToken(tok, testRepo)
	if !ok {
		t.Fatal("genuine notification token was rejected")
	}
	if user != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", user)
	}
	if gotExp != exp {
		t.Errorf("expected exp %d, got %d", exp, gotExp)
	}
}

// A session token is signed with the same key and, parsed as MyClaims, has an
// empty RepoID — so an empty repoID in the subscribe frame used to match it.
func TestParseNotifTokenRejectsSessionToken(t *testing.T) {
	claims := struct {
		Email string `json:"email"`
		jwt.RegisteredClaims
	}{
		Email: "mallory@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudSession},
		},
	}
	sessionTok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign session token: %v", err)
	}

	if _, _, ok := parseNotifToken(sessionTok, testRepo); ok {
		t.Error("session token was accepted as a notification token")
	}
	if _, _, ok := parseNotifToken(sessionTok, ""); ok {
		t.Error("session token with an empty repo id was accepted")
	}
}

func TestParseNotifTokenRejectsEmptyRepoID(t *testing.T) {
	tok, err := utils.GenNotifJWTToken("", "alice@example.com", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("failed to generate notif token: %v", err)
	}
	if _, _, ok := parseNotifToken(tok, ""); ok {
		t.Error("empty repo id was accepted")
	}
}

func TestParseNotifTokenRejectsRepoMismatch(t *testing.T) {
	tok, err := utils.GenNotifJWTToken(testRepo, "alice@example.com", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("failed to generate notif token: %v", err)
	}
	if _, _, ok := parseNotifToken(tok, "11111111-2222-3333-4444-555555555555"); ok {
		t.Error("token for a different repo was accepted")
	}
}

func TestParseNotifTokenPinsAlgorithm(t *testing.T) {
	claims := &utils.MyClaims{
		RepoID:   testRepo,
		UserName: "alice@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudNotif},
		},
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign HS512 token: %v", err)
	}

	if _, _, ok := parseNotifToken(hs512, testRepo); ok {
		t.Error("HS512 token was accepted despite the HS256 pin")
	}
}

func TestParseNotifTokenRejectsExpired(t *testing.T) {
	tok, err := utils.GenNotifJWTToken(testRepo, "alice@example.com", time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("failed to generate notif token: %v", err)
	}
	if _, _, ok := parseNotifToken(tok, testRepo); ok {
		t.Error("expired token was accepted")
	}
}

func TestRemoveSubscriptionStopsDelivery(t *testing.T) {
	Init()

	const repoID = "ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
	c := fakeClient()
	addSubscription(repoID, c)
	removeSubscription(repoID, c)

	NotifyRepoUpdate(repoID, "0000000000000000000000000000000000000000")

	select {
	case msg := <-c.wch:
		t.Errorf("unsubscribed client got message: %+v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}
