module github.com/SiloDrive/silo

go 1.26.0

toolchain go1.26.8

require (
	github.com/charmbracelet/bubbles v1.0.0
	github.com/charmbracelet/bubbletea v1.3.10
	github.com/charmbracelet/lipgloss v1.1.0
	github.com/getsentry/sentry-go v0.48.0
	github.com/google/uuid v1.6.0
	// Pinned. 1.8.x changes what a method mismatch answers when a PathPrefix
	// subrouter covers the same path: the subrouter matches the prefix, finds
	// no child route, and its 404 overwrites the 405 the earlier route had
	// already recorded. Seven routes here are registered on the root router
	// under /api/silo/v1 -- the unauthenticated ones, which sit outside
	// apiRouter precisely so RequireCredential does not apply -- and every one
	// of them would answer a wrong-method request with 404 instead of 405.
	// TestThePreLoginEndpointIsNotAGET catches it. A custom
	// MethodNotAllowedHandler does not help; by then the mismatch is no longer
	// reported at all. Bumping wants the route layout rethought, which is not
	// a dependency bump. See Silo #86.
	github.com/gorilla/mux v1.7.4
	github.com/gorilla/websocket v1.5.3
	github.com/sirupsen/logrus v1.9.3
	golang.org/x/crypto v0.57.0
	golang.org/x/term v0.46.0
	gopkg.in/ini.v1 v1.55.0
	modernc.org/sqlite v1.48.2
)

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/go-jose/go-jose/v4 v4.1.5
	golang.org/x/mod v0.41.0
	golang.org/x/oauth2 v0.37.0
)

require (
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.1 // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.9.0 // indirect
	github.com/clipperhouse/stringish v0.1.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.5.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/smartystreets/goconvey v1.8.1 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/telemetry v0.0.0-20260908163034-4bcc4b2ee518 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	golang.org/x/vuln v1.8.0 // indirect
	modernc.org/libc v1.70.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

tool golang.org/x/vuln/cmd/govulncheck
