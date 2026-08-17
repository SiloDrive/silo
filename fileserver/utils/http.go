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

func GetAuthorizationToken(h http.Header) string {
	auth := h.Get("Authorization")
	splitResult := strings.Split(auth, " ")
	if len(splitResult) > 1 {
		return splitResult[1]
	}
	return ""
}

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
func ClientIP(r *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		if addr := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); addr != "" {
			if ip := net.ParseIP(addr); ip != nil {
				return ip.String()
			}
		}
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

func HttpCommon(method, url string, header map[string][]string, reader io.Reader) (int, []byte, error) {
	header["Content-Type"] = []string{"application/json"}
	header["User-Agent"] = []string{"Seafile Server"}
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
