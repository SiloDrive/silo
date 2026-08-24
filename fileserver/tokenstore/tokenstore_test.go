package tokenstore

import (
	"sync"
	"testing"
	"time"
)

func TestCreateAndQueryToken(t *testing.T) {
	token := CreateToken("library-1", "obj-1", "download", "user@test.com", false)
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	info := QueryToken(token)
	if info == nil {
		t.Fatal("expected token to be found")
	}
	if info.LibraryID != "library-1" || info.ObjID != "obj-1" || info.Op != "download" || info.User != "user@test.com" {
		t.Errorf("unexpected token info: %+v", info)
	}
}

func TestQueryTokenReusable(t *testing.T) {
	token := CreateToken("library-1", "obj-1", "download", "user@test.com", false)

	// Non-one-time tokens should survive multiple queries
	for i := 0; i < 3; i++ {
		info := QueryToken(token)
		if info == nil {
			t.Fatalf("query %d: expected token to still exist", i+1)
		}
	}
}

func TestQueryTokenOneTime(t *testing.T) {
	token := CreateToken("library-1", "obj-1", "download", "user@test.com", true)

	info := QueryToken(token)
	if info == nil {
		t.Fatal("first query should return token")
	}

	info = QueryToken(token)
	if info != nil {
		t.Fatal("second query should return nil for one-time token")
	}
}

func TestQueryTokenNotFound(t *testing.T) {
	info := QueryToken("nonexistent-token")
	if info != nil {
		t.Fatal("expected nil for nonexistent token")
	}
}

func TestQueryTokenExpired(t *testing.T) {
	token := CreateToken("library-1", "obj-1", "download", "user@test.com", false)

	// Manually expire the token
	val, _ := tokens.Load(token)
	val.(*AccessInfo).ExpireTime = time.Now().Unix() - 1

	info := QueryToken(token)
	if info != nil {
		t.Fatal("expected nil for expired token")
	}

	// Should also be cleaned up from the map
	if _, ok := tokens.Load(token); ok {
		t.Fatal("expired token should have been deleted from map")
	}
}

func TestDeleteToken(t *testing.T) {
	token := CreateToken("library-1", "obj-1", "download", "user@test.com", false)

	DeleteToken(token)

	info := QueryToken(token)
	if info != nil {
		t.Fatal("expected nil after delete")
	}
}

// A one-time token exists so that a URL carrying it cannot be replayed.
// Redeeming it used to be a Load followed by a Delete, so two requests
// arriving together both saw it before either removed it, and both were
// served. Exactly one caller may win.
func TestQueryTokenOneTimeIsRedeemedOnce(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		token := CreateToken("library-1", "obj-1", "download", "user@test.com", true)

		const racers = 64
		start := make(chan struct{})
		results := make(chan *AccessInfo, racers)

		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- QueryToken(token)
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var served int
		for info := range results {
			if info != nil {
				served++
			}
		}
		if served != 1 {
			t.Fatalf("attempt %d: %d of %d racing requests were served a one-time token, want 1",
				attempt, served, racers)
		}
	}
}

// Redeeming one token must not disturb another.
func TestQueryTokenOneTimeIsPerToken(t *testing.T) {
	first := CreateToken("library-1", "obj-1", "download", "user@test.com", true)
	second := CreateToken("library-1", "obj-2", "download", "user@test.com", true)

	if info := QueryToken(first); info == nil {
		t.Fatal("the first token was not redeemable")
	}
	if info := QueryToken(second); info == nil {
		t.Error("redeeming one one-time token consumed another")
	}
}
