package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// TrustedProxyHops is how many proxies stand between a client and this server.
//
// It decides which entry of X-Forwarded-For is the client's, because the header
// is a path and not a value: each hop APPENDS the address it saw, so the list
// runs oldest first and the entries a client wrote itself sit at the front. One
// proxy means the last entry is the client. Two means the last is the inner
// proxy and the client is the one before it.
//
// One by default, which is the ordinary deployment -- Caddy or nginx in front,
// nothing else. Getting it too LOW is safe and getting it too high is not: too
// low attributes requests to the proxy nearest the client, which groups more
// clients into one bucket than it should, and too high reads an entry the
// client wrote and hands them a key they choose. So it never grows on its own,
// and an install with two proxies has to say so.
var TrustedProxyHops = 1

// ClientIP returns the address to attribute a request to.
//
// X-Forwarded-For and X-Real-Ip are honoured only when trustProxyHeaders is
// set, because anyone can send them. For logging that hardly matters; for
// anything that counts attempts per address it matters completely, since a
// forged header would give an attacker a fresh identity on every request and
// defeat the count entirely.
//
// The trade runs both ways: behind a reverse proxy with trustProxyHeaders
// off, every client arrives as the proxy's address and shares one bucket, so
// a deployment behind a proxy has to turn it on.
//
// Which is what made the entry this reads load-bearing. It took the FIRST one,
// and a proxy appends -- so on a request through one proxy the first entry is
// whatever the client put there, and an attacker minted a fresh rate-limit
// identity per request by writing one. The bug only bit with trustProxyHeaders
// on, which is to say on every deployment that had followed the advice above.
// See TrustedProxyHops for which entry is the client's.
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if ip := forwardedFor(r.Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
		// X-Real-Ip is a single value rather than a path, so there is no
		// position to get right -- and no way to tell a proxy's from a
		// client's either. It is trusted exactly as far as the proxy is
		// configured to overwrite it, which is why it is the fallback and not
		// the first question.
		if addr := strings.TrimSpace(r.Header.Get("X-Real-Ip")); addr != "" {
			if ip := net.ParseIP(addr); ip != nil {
				return ip.String()
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port to strip, or an address shape we do not recognise. Using it
		// verbatim keeps requests from one peer on one key, which is all a
		// rate limiter needs.
		return r.RemoteAddr
	}
	return host
}

// forwardedFor picks the client's entry out of an X-Forwarded-For list, or
// returns "" when the header says nothing this server has grounds to believe.
//
// Counting from the right, because that is the end this server can reason
// about: the last entry was written by the hop that talked to it. Anything to
// the left of the trusted hops was written by somebody further out, which on a
// one-proxy deployment means the client.
//
// An entry that is not an address is not skipped over to find one that is. A
// list whose trusted position holds junk is a proxy that is not doing what this
// server was told it does, and hunting leftwards for something parseable would
// walk straight into the part a client controls. Falling through to the peer
// address is the safe answer: over-grouping rather than a key an attacker
// chose.
func forwardedFor(header string) string {
	if strings.TrimSpace(header) == "" {
		return ""
	}
	hops := TrustedProxyHops
	if hops < 1 {
		hops = 1
	}
	entries := strings.Split(header, ",")
	i := len(entries) - hops
	if i < 0 {
		// Fewer entries than hops claimed: the request did not come the way
		// this server was told it would, so none of the list is trustworthy.
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(entries[i]))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func HttpCommon(method, url string, header map[string][]string, reader io.Reader) (int, []byte, error) {
	header["Content-Type"] = []string{"application/json"}
	header["User-Agent"] = []string{"Silo"}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	req.Header = header

	rsp, err := http.DefaultClient.Do(req)
	if err != nil {
		return http.StatusInternalServerError, nil, err
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		errMsg := parseErrorMessage(rsp.Body)
		return rsp.StatusCode, errMsg, fmt.Errorf("bad response %d for %s", rsp.StatusCode, url)
	}

	body, err := io.ReadAll(rsp.Body)
	if err != nil {
		return rsp.StatusCode, nil, err
	}

	return http.StatusOK, body, nil
}

func parseErrorMessage(r io.Reader) []byte {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil
	}
	var objs map[string]string
	err = json.Unmarshal(body, &objs)
	if err != nil {
		return body
	}
	errMsg, ok := objs["error_msg"]
	if ok {
		return []byte(errMsg)
	}

	return body
}
