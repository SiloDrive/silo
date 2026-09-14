package silod

import (
	"bytes"
	"math/rand"
	"net/http"
	"strings"
	"testing"
)

// The header these tests are about is the one that was missing.
//
// `GET entries/{path}` sent an ETag and a Last-Modified and no Cache-Control,
// which is the one combination RFC 9111 §4.2.2 licenses a cache to invent a
// freshness lifetime for — CFNetwork's heuristic being a tenth of the
// document's age. It cost a SiloDrive user three days: a directory listing was
// answered out of an on-disk cache with no request at all, the new child was
// not in that stale body, so the change was filtered out as a path the listing
// does not mention and the sync anchor advanced past the commit. There is no
// second chance after that — the delta is never asked for again.
//
// no-cache rather than no-store, because the ETag is a content hash and the
// 304 already costs one dirent lookup and reads no chunks: a client that wants
// to cache still can, it just has to ask first.
func wantsRevalidation(t *testing.T, got, what string) {
	t.Helper()
	if got == "" {
		t.Errorf("%s: no Cache-Control at all — with an ETag and a Last-Modified that is heuristically cacheable", what)
		return
	}
	if !strings.Contains(got, "no-cache") {
		t.Errorf("%s: Cache-Control = %q, want no-cache", what, got)
	}
	if !strings.Contains(got, "private") {
		t.Errorf("%s: Cache-Control = %q, want private — the response is scoped to one credential", what, got)
	}
}

// A directory listing is mutable, and this is the exact body that went stale.
func TestAListingMustBeRevalidated(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": ""}

	w := do(t, getEntry, acct, http.MethodGet, "/entries/", vars, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET listing = %d (%s), want 200", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") == "" {
		t.Fatal("no ETag on the listing; this test is measuring the wrong thing")
	}
	wantsRevalidation(t, w.Header().Get("Cache-Control"), "directory listing")
}

// The sharper half. A client hydrating a file fetches this URL, so a file
// edited on another machine could be materialised from the pre-edit bytes —
// wrong content rather than a missing entry, and nothing on either side says
// so.
func TestAFileBodyMustBeRevalidated(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "note.txt"}

	content := []byte("the bytes a stale cache would keep serving")
	w := do(t, putEntry, acct, http.MethodPut, "/entries/note.txt", vars, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	w = do(t, getEntry, acct, http.MethodGet, "/entries/note.txt", vars, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET body = %d (%s), want 200", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("body = %q, want %q", w.Body.Bytes(), content)
	}
	wantsRevalidation(t, w.Header().Get("Cache-Control"), "file body")
}

// A 304 updates the stored response's headers, so the policy has to ride on it
// too. Sent only on the 200, a cache that revalidates once keeps a stored copy
// whose freshness is still heuristic — the revalidation would teach it nothing.
func TestANotModifiedCarriesTheCachePolicy(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "note.txt"}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/note.txt", vars, []byte("hello"))
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", w.Code)
	}
	w = do(t, getEntry, acct, http.MethodGet, "/entries/note.txt", vars, nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag to revalidate against")
	}

	w = do(t, getEntry, acct, http.MethodGet, "/entries/note.txt", vars, nil,
		withHeader("If-None-Match", etag))
	if w.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", w.Code)
	}
	wantsRevalidation(t, w.Header().Get("Cache-Control"), "304 on a file body")
}

// objects/{id} is content-addressed, so max-age and immutable are right and
// stay. public is not: the route is libraries/{id}/objects/{id} and requires
// library authorization, and public tells a shared cache it may store the
// response and serve it to any requester at all — which is that check,
// bypassed.
func TestAnObjectIsCacheablePrivatelyAndNotPublicly(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}

	content := make([]byte, 3<<20)
	if _, err := rand.New(rand.NewSource(7)).Read(content); err != nil {
		t.Fatal(err)
	}
	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}
	id := string(bytes.TrimPrefix(bytes.Trim([]byte(w.Header().Get("ETag")), `"`), []byte(etagPrefix)))

	w = idReq(t, getObjectHandler, acct, http.MethodGet, "/objects/"+id,
		merge(map[string]string{"libraryid": libraryID}, "id", id), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET object = %d (%s), want 200", w.Code, w.Body.String())
	}

	cc := w.Header().Get("Cache-Control")
	if strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q; public lets a shared cache serve this to a requester who never passed the library check", cc)
	}
	if !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want private", cc)
	}
	// The id is the content hash, so this URL's representation genuinely
	// cannot change. That half was always right.
	if !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable kept", cc)
	}
	if !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want max-age kept", cc)
	}
}
