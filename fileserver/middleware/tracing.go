package middleware

import (
	"net/http"
	"strings"

	"github.com/dkam/silo/internal/observability"
	"github.com/gorilla/mux"
)

// NameTransaction renames the Sentry performance transaction after mux has
// matched a route, replacing the raw URL with the route's template.
//
// It has to run here rather than in the outer handler because the template is
// only known once matching has happened, and it matters because every sync
// route embeds a library id and most embed an object id too: named by URL,
// "GET /libraries/{libraryid}/chunks/{id}" would arrive as a few hundred thousand
// distinct endpoints of one request each, which is a lot of rows and no
// percentiles.
//
// Requests that match no route never reach a mux middleware, so a 404 keeps
// its URL-derived name — which is the one case where the raw path is what you
// want to see.
func NameTransaction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !observability.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if route := mux.CurrentRoute(r); route != nil {
			if tmpl, err := route.GetPathTemplate(); err == nil {
				observability.NameTransaction(r, routeName(tmpl))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// routeName turns a mux path template into something readable.
//
// Silo's sync routes pin their variables with regexes, so GetPathTemplate
// hands back the pattern as written — the permission check arrives as
// "/libraries/{libraryid:[\da-z]{8}-[\da-z]{4}-…}/permission-check{slash:\/?}". As a
// transaction name that is unreadable in a list and different again for every
// route that spells the same constraint slightly differently, so the
// constraints come off and the optional-trailing-slash variable, which is
// syntax rather than a path segment, goes entirely.
func routeName(template string) string {
	var b strings.Builder
	b.Grow(len(template))

	for i := 0; i < len(template); i++ {
		if template[i] != '{' {
			b.WriteByte(template[i])
			continue
		}
		name, rest, ok := splitVariable(template[i:])
		if !ok {
			// Unbalanced braces: leave the rest as it stands rather than
			// truncating a name into something that looks like a real route.
			b.WriteString(template[i:])
			return b.String()
		}
		if name != "slash" {
			b.WriteString("{" + name + "}")
		}
		i = len(template) - len(rest) - 1
	}
	return b.String()
}

// splitVariable reads one "{name}" or "{name:regexp}" off the front of s,
// counting braces so that the quantifiers inside a regexp — "[\da-z]{8}" — do
// not look like the end of the variable. It returns the name and whatever
// follows the closing brace.
func splitVariable(s string) (name, rest string, ok bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				name, _, _ = strings.Cut(s[1:i], ":")
				return name, s[i+1:], true
			}
		}
	}
	return "", s, false
}
