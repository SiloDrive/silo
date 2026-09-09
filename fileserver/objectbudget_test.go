package silod

import (
	"bytes"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
)

// An object PUT was bounded at store.MaxManifestBytes plus framing — a
// gibibyte and a megabyte — and nothing bounded how many of them ran at once.
// AES-GCM is one-shot, so a whole object is buffered to be sealed or opened,
// and the same is true on the way back out: every read of a manifest holds all
// of it in memory, and a Range request re-reads it.
//
// So the arithmetic that matters is not the size of one object but the size of
// one object times the number of requests an attacker chooses to send. Eight
// was enough to ask for forty gigabytes.
func TestOneRequestCannotAskForMoreMemoryThanTheServerHas(t *testing.T) {
	libraryID, acct := testLibrary(t)

	old := option.MaxBufferedObjectBytes
	option.MaxBufferedObjectBytes = 1 << 10
	t.Cleanup(func() { option.MaxBufferedObjectBytes = old })

	content := bytes.Repeat([]byte("a"), 8<<10)
	m := &store.Manifest{FileSize: int64(len(content)), Inline: content}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	id := store.ObjectID(manifestBytes)
	vars := map[string]string{"libraryid": libraryID}

	w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+id.String(),
		merge(vars, "id", id.String()), manifestBytes, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT of an object larger than the whole buffer budget = %d (%s), want 503",
			w.Code, w.Body.String())
	}
}

// And the budget has to come back, or the first large request takes the server
// down with it more thoroughly than the attack would have.
func TestTheBufferBudgetIsReturned(t *testing.T) {
	libraryID, acct := testLibrary(t)

	old := option.MaxBufferedObjectBytes
	option.MaxBufferedObjectBytes = 64 << 10
	t.Cleanup(func() { option.MaxBufferedObjectBytes = old })

	vars := map[string]string{"libraryid": libraryID}
	for i := 0; i < 20; i++ {
		content := bytes.Repeat([]byte{byte(i)}, 8<<10)
		m := &store.Manifest{FileSize: int64(len(content)), Inline: content}
		manifestBytes, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		id := store.ObjectID(manifestBytes)
		w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+id.String(),
			merge(vars, "id", id.String()), manifestBytes, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT %d of 20, each well inside the budget = %d (%s), want 201",
				i, w.Code, w.Body.String())
		}
		// And it can be read back, which is the other half of the budget.
		g := idReq(t, getObjectHandler, acct, http.MethodGet, "/objects/"+id.String(),
			merge(vars, "id", id.String()), nil, nil)
		if g.Code != http.StatusOK {
			t.Fatalf("GET %d = %d (%s), want 200", i, g.Code, g.Body.String())
		}
	}
}

// Concurrent readers of one large object share the budget rather than each
// taking their own copy of it.
func TestConcurrentObjectReadsShareTheBudget(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	content := bytes.Repeat([]byte("m"), 32<<10)
	m := &store.Manifest{FileSize: int64(len(content)), Inline: content}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	id := store.ObjectID(manifestBytes)
	if w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+id.String(),
		merge(vars, "id", id.String()), manifestBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("seeding the object = %d (%s)", w.Code, w.Body.String())
	}

	// Room for two of them at a time, and twenty asking.
	old := option.MaxBufferedObjectBytes
	option.MaxBufferedObjectBytes = int64(len(manifestBytes)) * 2
	t.Cleanup(func() { option.MaxBufferedObjectBytes = old })

	var refused atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			g := idReq(t, getObjectHandler, acct, http.MethodGet, "/objects/"+id.String(),
				merge(vars, "id", id.String()), nil, nil)
			switch g.Code {
			case http.StatusOK:
			case http.StatusServiceUnavailable:
				refused.Add(1)
			default:
				t.Errorf("GET = %d (%s), want 200 or 503", g.Code, g.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	t.Logf("%d of 20 concurrent reads were refused", refused.Load())
}
