// Package silod is the Silo file server daemon.
package silod

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/admin"
	"github.com/SiloDrive/silo/fileserver/adminui"
	"github.com/SiloDrive/silo/fileserver/api"
	"github.com/SiloDrive/silo/fileserver/authmgr"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/dbutil"
	"github.com/SiloDrive/silo/fileserver/invite"
	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/notif"
	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/fileserver/serversecret"
	"github.com/SiloDrive/silo/fileserver/setup"
	"github.com/SiloDrive/silo/fileserver/share"
	"github.com/SiloDrive/silo/fileserver/utils"
	"github.com/SiloDrive/silo/internal/observability"
	"github.com/SiloDrive/silo/internal/xdg"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"

	"net/http/pprof"
)

var dataDir, absDataDir string
var configFile string
var bindAddr string
var logFile, absLogFile string
var pidFilePath string
var logFp *os.File

var siloPair *dbutil.DBPair

var httpServer *http.Server
var shutdownDone = make(chan struct{})

var logToStdout bool
var debugLog bool

func init() {
	log.SetFormatter(&LogFormatter{})
}

const (
	timestampFormat = "[2006-01-02 15:04:05] "
)

// HTTP server limits.
//
// Only the two timeouts that are safe for a file server are set.
// ReadTimeout and WriteTimeout are deliberately left at zero: they bound the
// whole request body read and the whole response write respectively, and both
// uploads and downloads here stream arbitrarily large files through the same
// handler, so any finite value would sever legitimate transfers of a big
// enough file on a slow enough link. ReadHeaderTimeout closes the Slowloris
// hole those two would otherwise have covered, since it bounds the part an
// attacker controls without touching the body.
//
// IdleTimeout must be set explicitly: left at zero it falls back to
// ReadTimeout, which is also zero here, so keep-alive connections would idle
// forever. WebSocket connections on /notification are unaffected — once
// Upgrade hijacks the connection these no longer apply.
const (
	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20 // 1MB, matching net/http's default
)

type LogFormatter struct{}

func (f *LogFormatter) Format(entry *log.Entry) ([]byte, error) {
	levelStr := entry.Level.String()
	if levelStr == "fatal" {
		levelStr = "ERROR"
	} else {
		levelStr = strings.ToUpper(levelStr)
	}
	level := fmt.Sprintf("[%s] ", levelStr)
	appName := ""
	if logToStdout {
		appName = "[fileserver] "
	}
	buf := make([]byte, 0, len(appName)+len(timestampFormat)+len(level)+len(entry.Message)+1)
	if logToStdout {
		buf = append(buf, appName...)
	}
	buf = entry.Time.AppendFormat(buf, timestampFormat)
	buf = append(buf, level...)
	buf = append(buf, entry.Message...)
	buf = append(buf, '\n')
	return buf, nil
}

// resolvePaths fills absDataDir and configFile from the -d/-C flags, the
// environment and the XDG defaults, in that order. `serve` and `gc` share it
// so they cannot disagree about which data directory they are pointed at —
// a divergence would have gc reclaiming objects from the wrong store.
func resolvePaths() error {
	if dataDir == "" {
		dataDir = os.Getenv("SILO_DATA_DIR")
	}
	if dataDir == "" {
		xdgDefault, err := xdg.DataHome("silo")
		if err != nil {
			return fmt.Errorf("cannot determine data directory: %v; use -d or set SILO_DATA_DIR", err)
		}
		dataDir = xdgDefault
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory %s: %v", dataDir, err)
	}
	var err error
	absDataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("failed to convert data dir to absolute path: %v", err)
	}

	if configFile == "" {
		if xdgConf, err := xdg.ConfigHome("silo"); err == nil {
			candidate := filepath.Join(xdgConf, "silo.conf")
			if _, err := os.Stat(candidate); err == nil {
				configFile = candidate
			}
		}
	}
	return nil
}

// commandFlags returns the flag set every subcommand starts from. -d and -C
// mean the same thing to all of them, so they are declared once: a subcommand
// that spelled either differently would point at a different data directory
// than the server it is meant to operate on.
func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet("silo "+name, flag.ContinueOnError)
	flags.StringVar(&configFile, "C", "", "path to config file (optional)")
	flags.StringVar(&dataDir, "d", "", "data directory (default: $SILO_DATA_DIR or ~/.local/share/silo)")
	return flags
}

// parseCommandArgs parses a subcommand's arguments and returns the positional
// ones. done is true when -h was handled and the command should return without
// doing anything.
//
// The trailing-flag check belongs here rather than at each command because
// forgetting it is silent, and gc had already forgotten it. flag.Parse stops
// at the first positional and hands the rest back, so "-d /srv/silo" written
// at the end is not a parse error — it is ignored, and the command runs
// against the default data directory. For a command that reports what a
// user's tokens are, writes a backup, or deletes objects, being pointed at the
// wrong deployment without saying so is worse than refusing.
func parseCommandArgs(name string, flags *flag.FlagSet, args []string) ([]string, bool, error) {
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil, true, nil
		}
		return nil, false, err
	}
	rest := flags.Args()
	for _, arg := range rest {
		if len(arg) > 1 && arg[0] == '-' {
			return nil, false, fmt.Errorf(
				"flag %s must come before the arguments, as in: silo %s %s <args>", arg, name, arg)
		}
	}
	return rest, false, nil
}

// openStores resolves the paths, loads the options and opens the databases —
// the preamble a subcommand needs before it can read anything. The order is
// not free: loadDatabases migrates, and the migration reads the API token TTL
// that LoadFileServerOptions sets.
func openStores() error {
	if err := resolvePaths(); err != nil {
		return err
	}
	// Before anything opens a library: a store whose key is missing cannot be
	// read at all, and finding that out here means one error that says so
	// rather than a failure per object, later, from whatever happened to ask
	// first. On a first start this is what generates the key.
	if err := objstore.EnsureKey(absDataDir); err != nil {
		return err
	}
	option.LoadFileServerOptions(configFile)
	// Before loadDatabase and before libmgr, because a library opened here is
	// a packStore created here, and two of the pack settings are read once at
	// that moment. See objstore.Configure.
	if err := objstore.Configure(option.Packs); err != nil {
		return err
	}
	loadDatabase()
	libmgr.Init(siloPair.Read, siloPair.Write, dataDir)
	return nil
}

// serveHandler wraps a router in the middleware every request passes through.
//
// A function rather than a few lines inside RunServer because RunServer is not
// the only thing that stands this router up: the wire tests build one too, and
// while this lived inline they were exercising a handler stack that was not
// the one the server runs. That is the shape of gap where a middleware is
// added, every test passes, and it is installed nowhere a test can see -- which
// is exactly what happened to the traffic counters until a test asked the
// server how many bytes it had served.
//
// Order matters and is the reason for each line. DebugLogger is innermost and
// conditional, because it is a debugging aid. CountTraffic is unconditional:
// the counters answer a question an operator asks about a server running
// normally, so hanging them off a debug-only wrapper would mean they read zero
// on every install that would ask. observability is outermost, so a panic in
// any of the above is still reported and every request is timed from the
// moment it arrives rather than from after the prefix rewrite.
func serveHandler(router http.Handler, debugLog bool) http.Handler {
	handler := router
	if debugLog {
		handler = middleware.DebugLogger(handler)
	}
	handler = middleware.CountTraffic(handler)
	return observability.Middleware(handler)
}

// DatabaseName is the single SQLite file every table lives in, relative to
// the data directory.
const DatabaseName = "silo.db"

// migrateOnOpen is set by the one caller that holds the data directory lock
// and may therefore change the shape of the database: the server. Every other
// command opens the database beside a server that may be running, and gets
// Prepare, which refuses a database that is behind rather than migrating it
// under that server's feet.
var migrateOnOpen bool

func loadDatabase() {
	dbPath := filepath.Join(absDataDir, DatabaseName)

	var err error
	siloPair, err = dbutil.OpenSQLite(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	if migrateOnOpen {
		if _, err := dbutil.Migrate(siloPair.Write); err != nil {
			log.Fatalf("Failed to migrate the database: %v", err)
		}
	} else if err := dbutil.Prepare(siloPair.Write); err != nil {
		log.Fatalf("Failed to open the database: %v", err)
	}

	log.Infof("Using database %s", dbPath)
}

func writePidFile(pid_file_path string) error {
	// O_TRUNC: a stale pidfile may be longer than the new pid string, so
	// without truncating we'd leave junk bytes after the pid.
	file, err := os.OpenFile(pid_file_path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0664)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	_, err = fmt.Fprintf(file, "%d", os.Getpid())
	return err
}

func removePidfile(pid_file_path string) error {
	if pid_file_path == "" {
		return nil
	}
	err := os.Remove(pid_file_path)
	if err != nil {
		return err
	}
	return nil
}

// Run starts the fileserver daemon. It parses its own flags from args (which
// should be everything after the `serve` subcommand), initializes databases
// and managers, and blocks until shutdown. It returns nil on graceful exit;
// any error during argument parsing or startup is returned to the caller.
// Note: much of the server bootstrap still uses log.Fatalf for fatal conditions,
// which calls os.Exit directly — that behavior is unchanged from the previous
// standalone binary.
func Run(args []string) error {
	fs := commandFlags("serve")
	fs.StringVar(&bindAddr, "b", "", "bind address as host or host:port (default: $SILO_HOST:$SILO_PORT or 127.0.0.1:8082)")
	fs.StringVar(&logFile, "l", "", "log file path (default: stdout)")
	fs.StringVar(&pidFilePath, "P", "", "pid file path")
	fs.BoolVar(&debugLog, "debug", false, "log every HTTP request (method, path, status, duration)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	if err := resolvePaths(); err != nil {
		log.Fatalf("%v", err)
	}

	// Before anything opens a pack, and before the pidfile, because a second
	// server on one data directory destroys the first's open pack and takes
	// every acknowledged frame in it with it. See objstore.LockDataDir for
	// how. Held for the life of the process, and dropped by the kernel if the
	// process does not get to drop it itself.
	//
	// Ahead of the pidfile so a refused start does not overwrite the running
	// server's, which would leave an operator holding the pid of a process
	// that exited.
	dirLock, err := objstore.LockDataDir(absDataDir)
	if err != nil {
		log.Fatalf("%v", err)
	}
	// Deferred rather than released at the end of the shutdown sequence,
	// because Run returns only once handleSignals has closed shutdownDone --
	// so this is the last thing that happens, after the packs are sealed and
	// the database is closed. The lock says this process owns the store, and
	// it owns it until it has finished putting it down.
	defer func() {
		if err := dirLock.Release(); err != nil {
			log.Warnf("Failed to release the data directory lock: %v", err)
		}
	}()

	if pidFilePath != "" {
		if err := writePidFile(pidFilePath); err != nil {
			log.Fatalf("Failed to write pid file %s: %v", pidFilePath, err)
		}
	}

	log.Infof("Data directory: %s", absDataDir)

	// Logging: default to stdout. Use -l to write to a file instead.
	if logFile != "" && logFile != "-" {
		var err error
		absLogFile, err = filepath.Abs(logFile)
		if err != nil {
			log.Fatalf("Failed to convert log file path to absolute path: %v", err)
		}
		fp, err := os.OpenFile(absLogFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
		if err != nil {
			log.Fatalf("Failed to open or create log file: %v", err)
		}
		logFp = fp
		log.SetOutput(fp)
		logToStdout = false
		if err := utils.RedirectStderr(logFp); err != nil {
			log.Warnf("Panics and runtime errors will not reach the log file: %v", err)
		}
	} else {
		logToStdout = true
	}

	// See openStores: the same check, for the path that does not go through
	// it. A server that cannot read its own store should say so and stop, not
	// accept requests and fail every one of them.
	//
	// After the log destination is settled, because on a first start this is
	// what generates storage.key and prints the back-it-up warning — and that
	// warning says "printed once", so the one run that emits it is the one
	// run that must not send it to a stderr nobody is reading.
	if err := objstore.EnsureKey(absDataDir); err != nil {
		log.Fatalf("%v", err)
	}

	// After the log destination is settled, so the "reporting to" line lands
	// in the same place as everything else, and before the rest of startup, so
	// a failure to load config, open a database or create the admin is a
	// failure that gets reported rather than one that only exists in a log
	// nobody is reading.
	defer observability.Init("fileserver", option.Version)()

	option.LoadFileServerOptions(configFile)
	// Run does not go through openStores, so the same rule applies here: see
	// objstore.Configure.
	if err := objstore.Configure(option.Packs); err != nil {
		log.Fatalf("Failed to apply the [storage] settings: %v", err)
	}
	// After the options, so the flag beats both SILO_HOST and the config file.
	// Same precedence as -d over SILO_DATA_DIR: what you typed on this command
	// line wins over what the environment happens to be carrying.
	//
	// The port moves only when -b carried one. "-b 0.0.0.0" is the form this
	// flag has always taken, and an install using it keeps the port SILO_PORT
	// or silo.conf gave it.
	if bindAddr != "" {
		host, port, portSet, err := parseBindAddr(bindAddr)
		if err != nil {
			return err
		}
		option.Host = host
		if portSet {
			option.Port = port
		}
	}
	// The lock above is what makes this safe; see migrateOnOpen.
	migrateOnOpen = true
	loadDatabase()

	level, err := log.ParseLevel(option.LogLevel)
	if err != nil {
		log.Info("use the default log level: info")
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(level)
	}

	libmgr.Init(siloPair.Read, siloPair.Write, dataDir)

	share.Init(siloPair.Read, siloPair.Write)

	account.Init(siloPair.Read, siloPair.Write)
	admin.Init(siloPair.Read, siloPair.Write)
	authmgr.Init(siloPair.Read, siloPair.Write)
	api.Init(siloPair.Read, siloPair.Write)
	api.StartLoginLimiterCleanup()
	credential.Init(siloPair.Read, siloPair.Write)
	credential.StartCleanup()
	serversecret.Init(siloPair.Read, siloPair.Write)

	setup.Init(siloPair.Read, siloPair.Write)
	invite.Init(siloPair.Read, siloPair.Write)

	// Mint the setup token, if this server has never had an account.
	//
	// The server no longer invents an account for itself. It used to, because a
	// server nobody can log in to is not a useful server -- but the account it
	// invented had an address the operator did not choose and a password only a
	// log line ever held, and the alternative was a password sitting in
	// docker-compose.yml forever after the one boot that needed it. The token
	// replaces both: it proves whoever holds it can read this host, and the
	// operator picks their own address and password.
	//
	// Reprinted on every boot until it is claimed, deliberately. It is the same
	// token each time, so an operator who scrolled past it does not have to
	// wonder which of two strings is live.
	setupCtx, cancelSetup := option.WithDBTimeout(context.Background())
	setupToken, err := setup.Ensure(setupCtx)
	cancelSetup()
	if err != nil {
		log.Fatalf("Failed to prepare the setup token: %v", err)
	}
	// The zero token is how Ensure says this server already has an account.
	if !setupToken.IsZero() {
		logSetupToken(setupToken)
	}

	notif.Init()

	router := newHTTPRouter()

	httpServer = new(http.Server)
	httpServer.Addr = listenAddr()
	httpServer.Handler = serveHandler(router, debugLog)
	httpServer.ReadHeaderTimeout = readHeaderTimeout
	httpServer.IdleTimeout = idleTimeout
	httpServer.MaxHeaderBytes = maxHeaderBytes

	// Start signal handlers AFTER httpServer is fully constructed: the
	// shutdown handler reads httpServer concurrently, and goroutine creation
	// is the happens-before edge that makes the writes above visible.
	go handleSignals()
	go handleUser1Signal()

	// Bind before reporting, and before backgrounding the serve loop. A
	// listener opened inside the goroutine could only report its failure by
	// logging, and Run would then block on shutdownDone forever — a process
	// that is alive, says it is listening, and serves nothing. Under systemd
	// that unit stays "active", so Restart=on-failure never fires. Binding
	// here turns a bad address or a taken port into a returned error, which is
	// the difference between a typo in -b costing a second and costing an
	// afternoon.
	ln, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		return fmt.Errorf("failed to bind %s: %v", httpServer.Addr, err)
	}

	// Reported as configured rather than as ln.Addr(), which renders a 0.0.0.0
	// bind as "[::]" — accurate, since the wildcard listener is dual-stack,
	// but not what anyone typed, and it would disagree with the warning below.
	log.Printf("Silo server listening on http://%s", displayAddr())
	warnIfExposedWithoutTLS()

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Errorf("File server exiting: %v", err)
		}
	}()

	<-shutdownDone
	return nil
}

// logSetupToken prints the token that creates this server's first account.
//
// Several lines at warning level, on purpose. An operator scanning a first boot
// has to be able to find it, and the sentence saying what it is for is what
// stops it being mistaken for a password.
//
// **Warn, not Error, and that is not a style choice.** The Sentry hook fires on
// Panic, Fatal and Error only (internal/observability, logrusHook.Levels), so
// the level is what keeps this string on the machine it was printed on. There
// is no PII setting that would substitute: the hook builds its event from the
// log message, and the SDK's scrubbing only ever touches the request. Raising
// this to Error would ship the token to an error reporter and nothing would
// fail to compile.
func logSetupToken(tok setup.Token) {
	log.Warn("This server has no accounts. Create the first one with this setup token:")
	log.Warnf("    setup token: %s", tok)
	log.Warn("Run `silo tui`, enter the email and password you want, and paste it in.")
	log.Warn("It stops working the moment an account exists. `silo setup-token` reprints it.")
}

// warnIfExposedWithoutTLS says so when the server is reachable from off the
// machine over plaintext. Every credential Silo uses is a bearer token sent in
// a header, so anyone on the path can lift one and keep it.
//
// Silo speaks plaintext and only plaintext: TLS belongs to the reverse proxy
// in front of it. It is a warning rather than a refusal because the server
// cannot tell from here whether a proxy is there.
func warnIfExposedWithoutTLS() {
	if !exposedWithoutTLS() {
		return
	}
	log.Warnf("Listening on %s without TLS. Passwords and tokens will cross the "+
		"network in clear text — put a TLS reverse proxy in front, and set "+
		"SILO_TRUST_PROXY_HEADERS=true so rate limiting sees the real client.",
		displayAddr())
}

func handleSignals() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-signalChan

	log.Info("shutdown signal received, draining HTTP server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if httpServer != nil {
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Warnf("HTTP server shutdown error: %v", err)
		}
	}

	// Before the database, because sealing writes files that the store must be
	// able to describe afterwards, and after the HTTP drain, because a request
	// still in flight may still be appending frames.
	if err := objstore.Close(); err != nil {
		log.Warnf("Failed to seal open packs: %v", err)
	}

	checkpointAndClose(siloPair)

	if err := removePidfile(pidFilePath); err != nil {
		log.Warnf("Failed to remove pid file: %v", err)
	}

	log.Info("shutdown complete")
	close(shutdownDone)
}

func checkpointAndClose(pair *dbutil.DBPair) {
	if pair == nil {
		return
	}
	if _, err := pair.Write.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Warnf("WAL checkpoint failed: %v", err)
	}
	if err := pair.Close(); err != nil {
		log.Warnf("DB close failed: %v", err)
	}
}

func handleUser1Signal() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGUSR1)

	for {
		<-signalChan
		logRotate()
	}
}

func logRotate() {
	if logToStdout {
		return
	}
	// reopen fileserver log
	fp, err := os.OpenFile(absLogFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		log.Fatalf("Failed to reopen fileserver log: %v", err)
	}
	log.SetOutput(fp)
	if logFp != nil {
		_ = logFp.Close()
		logFp = fp
	}

	if err := utils.RedirectStderr(logFp); err != nil {
		log.Warnf("Panics and runtime errors will not reach the reopened log file: %v", err)
	}
}

func newHTTPRouter() *mux.Router {
	r := mux.NewRouter()
	// Registered only when there is somewhere to report to, so that with no
	// DSN configured there is no extra middleware in the chain at all rather
	// than one that returns early.
	if observability.Enabled() {
		r.Use(middleware.NameTransaction)
	}
	// in-process notification-server WebSocket endpoint
	//
	// RequireSocketCredential rather than RequireCredential: the socket names
	// no library in its path, so the door check would refuse every scoped
	// credential a mount holds. Each subscribe frame names its own library and
	// is checked against this credential instead.
	if option.EnableNotification {
		r.Handle("/notification", middleware.RequireSocketCredential(http.HandlerFunc(notif.Handler)))
	}

	// pprof
	r.Handle("/debug/pprof", &profileHandler{http.HandlerFunc(pprof.Index)})
	r.Handle("/debug/pprof/cmdline", &profileHandler{http.HandlerFunc(pprof.Cmdline)})
	r.Handle("/debug/pprof/profile", &profileHandler{http.HandlerFunc(pprof.Profile)})
	r.Handle("/debug/pprof/symbol", &profileHandler{http.HandlerFunc(pprof.Symbol)})
	r.Handle("/debug/pprof/heap", &profileHandler{pprof.Handler("heap")})
	r.Handle("/debug/pprof/block", &profileHandler{pprof.Handler("block")})
	r.Handle("/debug/pprof/goroutine", &profileHandler{pprof.Handler("goroutine")})
	r.Handle("/debug/pprof/threadcreate", &profileHandler{pprof.Handler("threadcreate")})
	r.Handle("/debug/pprof/trace", &traceHandler{})

	// Management API
	r.HandleFunc("/api/silo/v1/auth/login", api.LoginHandler).Methods("POST")
	// Claiming a server that has no accounts. Unauthenticated because there is
	// nothing yet to authenticate against: this is the request that creates the
	// first account. It is guarded by the setup token instead, and refuses with
	// 409 the moment an account exists. See api.SetupHandler.
	r.HandleFunc("/api/silo/v1/auth/setup", api.SetupHandler).Methods("POST")
	// The pre-login parameters endpoint, which is unauthenticated because
	// nothing can be authenticated yet: a client needs these to turn a
	// password into the value it sends. See api.KDFParamsHandler for the two
	// hazards that keeps it from being an account-enumeration oracle.
	r.HandleFunc("/api/silo/v1/auth/kdf", api.KDFParamsHandler).Methods("POST")
	r.HandleFunc("/api/silo/v1/server-info", api.ServerInfoHandler).Methods("GET")
	// Redeeming an invite. Unauthenticated for setup's reason -- there is
	// nothing yet to authenticate as, because the account this request activates
	// opens no lane until it does -- and guarded by the invite token instead.
	// See api.RedeemInviteHandler.
	r.HandleFunc("/api/silo/v1/auth/redeem", api.RedeemInviteHandler).Methods("POST")
	// The administrative page. Unauthenticated because it carries no data: it
	// is a form and two empty tables, and every number on it arrives from a
	// fetch the browser makes with a credential the person typed in. Gating the
	// shell would only mean an operator could not reach the login box.
	r.HandleFunc("/admin", adminui.Handler).Methods("GET")
	// Logging out is about the credential presenting it rather than about the
	// account, so it is mounted outside the subrouter with the lane that does
	// not apply the narrowing. A mount cut to one library must be able to sign
	// itself out; see middleware.RequireOwnCredential.
	r.Handle("/api/silo/v1/auth/logout",
		middleware.RequireOwnCredential(http.HandlerFunc(api.LogoutHandler))).Methods("POST")
	// Renewal is about the credential presenting it for the same reason logout
	// is -- it is that row asking for its own successor -- so it takes the same
	// lane. A mount cut to one library must be able to replace its credential
	// without an operator, exactly as it must be able to sign itself out.
	r.Handle("/api/silo/v1/auth/renew",
		middleware.RequireOwnCredential(http.HandlerFunc(api.RenewHandler))).Methods("POST")
	apiRouter := r.PathPrefix("/api/silo/v1").Subrouter()
	apiRouter.Use(middleware.RequireCredential)
	// Before the handlers, so the ones with a policy of their own overwrite it
	// and the rest cannot be left to a cache's own arithmetic. See
	// middleware.DefaultCachePolicy.
	apiRouter.Use(middleware.DefaultCachePolicy)
	// Both of these are account-wide, so both take the narrowing: a credential
	// scoped to one library is refused them here rather than in the handler.
	apiRouter.HandleFunc("/auth/logout/everywhere", api.LogoutEverywhereHandler).Methods("POST")
	apiRouter.HandleFunc("/auth/password", api.ChangePasswordHandler).Methods("POST")
	apiRouter.HandleFunc("/account", api.AccountHandler).Methods("GET")
	apiRouter.HandleFunc("/account/keys", api.GetAccountKeysHandler).Methods("GET")
	apiRouter.HandleFunc("/account/keys", api.PutAccountKeysHandler).Methods("PUT")
	apiRouter.HandleFunc("/account/keys/recovery/{ordinal:[0-9]+}",
		api.DeleteRecoveryWrapHandler).Methods("DELETE")
	apiRouter.HandleFunc("/account/usage", api.AccountUsageHandler).Methods("GET")
	// Account-wide, so both take the narrowing: seeing or revoking every
	// credential an account holds is strictly wider than a scope cut to one
	// library. Contrast auth/logout and auth/renew above, which are mounted
	// outside this subrouter because they are about the row presenting them --
	// a mount cut to one library must still be able to sign itself out and
	// replace its own credential.
	apiRouter.HandleFunc("/account/credentials", api.ListCredentialsHandler).Methods("GET")
	apiRouter.HandleFunc("/account/credentials/{id}", api.RevokeCredentialHandler).Methods("DELETE")

	// The administrative surface. Each route names the capability that opens
	// it at the mount rather than inside the handler, because the pairing is
	// the design: a subtree gate would be a single admin bit wearing a table's
	// clothes. docs/plans/admin.md § The HTTP surface holds the same table.
	//
	// Mounted inside apiRouter, so a request with no credential is refused
	// there with 401 and one with a credential but not enough authority is
	// refused here with 403. A library-scoped credential is refused by the
	// narrowing before either: these routes name no library, which is exactly
	// what the narrowing is for.
	adminRoute := func(path string, capability admin.Capability, h http.HandlerFunc, methods ...string) {
		apiRouter.Handle(path, middleware.RequireAdmin(capability, h)).Methods(methods...)
	}
	adminRoute("/admin/accounts", admin.CapUsers, api.ListAdminAccountsHandler, "GET")
	adminRoute("/admin/accounts", admin.CapUsers, api.CreateAdminAccountHandler, "POST")
	adminRoute("/admin/accounts/{id}/active", admin.CapUsers, api.SetAdminAccountActiveHandler, "POST")
	adminRoute("/admin/accounts/{id}/password", admin.CapPasswords, api.SetAdminAccountPasswordHandler, "POST")
	adminRoute("/admin/accounts/{id}/quota", admin.CapQuota, api.GetAdminAccountQuotaHandler, "GET")
	adminRoute("/admin/accounts/{id}/quota", admin.CapQuota, api.SetAdminAccountQuotaHandler, "PUT")
	adminRoute("/admin/accounts/{id}/role", admin.CapGrant, api.SetAdminAccountRoleHandler, "PUT")
	adminRoute("/admin/accounts/{id}/caps", admin.CapGrant, api.SetAdminAccountCapabilitiesHandler, "PUT")
	// Gated by quota rather than users. A listing of every library with its
	// owner and its size is usage information, which is what that capability
	// already means -- and it is the view the server-level ceiling needs, since
	// a refusal at that ceiling is otherwise a refusal with no way to see what
	// filled it.
	// Gated by users, because an invite is an account: minting one writes the
	// row an address will belong to, and redeeming it is the only registration
	// path there is. See api.CreateInviteHandler for why an invite naming the
	// admin role is not an escalation past that gate.
	adminRoute("/admin/invites", admin.CapUsers, api.ListInvitesHandler, "GET")
	adminRoute("/admin/invites", admin.CapUsers, api.CreateInviteHandler, "POST")
	adminRoute("/admin/invites/{id}", admin.CapUsers, api.RevokeInviteHandler, "DELETE")
	adminRoute("/admin/libraries", admin.CapQuota, api.ListAdminLibrariesHandler, "GET")
	adminRoute("/admin/storage", admin.CapRetention, adminStorageHandler, "GET")

	apiRouter.HandleFunc("/libraries", api.ListLibrariesHandler).Methods("GET")
	apiRouter.HandleFunc("/libraries", api.CreateLibraryHandler).Methods("POST")
	apiRouter.HandleFunc("/libraries/{libraryid}", api.DeleteLibraryHandler).Methods("DELETE")
	apiRouter.HandleFunc("/libraries/{libraryid}", patchLibraryHandler).Methods("PATCH")
	apiRouter.HandleFunc("/libraries/{libraryid}/key", api.LibraryKeyHandler).Methods("GET")
	// The share surface. Whole-library grants, managed by the owner: see
	// api.ownedLibrary for why that is the whole of the access rule, and
	// docs/plans/sharing.md § The grant model for what a grant is. The
	// principal is one path segment and carries a colon, which no route above
	// it can be confused with.
	apiRouter.HandleFunc("/libraries/{libraryid}/shares", api.ListSharesHandler).Methods("GET")
	apiRouter.HandleFunc("/libraries/{libraryid}/shares", api.CreateShareHandler).Methods("POST")
	apiRouter.HandleFunc("/libraries/{libraryid}/shares/{principal}", api.DeleteShareHandler).Methods("DELETE")
	apiRouter.HandleFunc("/libraries/{libraryid}/changes", api.ChangesHandler).Methods("GET")
	apiRouter.HandleFunc("/libraries/{libraryid}/commits", api.CommitsHandler).Methods("GET")
	// The chunk surface. "missing" cannot collide with a chunk id — the id
	// route only matches 64 hex characters — but it is listed first anyway,
	// because relying on a regex to keep two routes apart is the kind of thing
	// that stops being true when someone loosens the regex.
	apiRouter.HandleFunc("/libraries/{libraryid}/batch", batchHandler).Methods("POST")
	apiRouter.HandleFunc("/libraries/{libraryid}/chunks/missing", chunksMissingHandler).Methods("POST")
	// "fetch" is subject to the same note as "missing": it cannot collide with
	// a chunk id, and it is listed before the id route regardless.
	apiRouter.HandleFunc("/libraries/{libraryid}/chunks/fetch", chunksFetchHandler).Methods("POST")
	// The collection itself, POST only: many chunks in one framed body. It
	// names no id because it carries several, which is also why it cannot
	// collide with the id route below — that one has a segment where this has
	// none.
	apiRouter.HandleFunc("/libraries/{libraryid}/chunks", chunksUploadHandler).Methods("POST")
	// The id-addressed surface. A chunk id is sixty-four hex characters, so
	// the id and the route regex cannot collide — the width is the format, not
	// a convention. See objects.go.
	apiRouter.HandleFunc("/libraries/{libraryid}/chunks/{id:[0-9a-f]{64}}", getChunkHandler).Methods("GET", "HEAD")
	apiRouter.HandleFunc("/libraries/{libraryid}/chunks/{id:[0-9a-f]{64}}", putChunkHandler).Methods("PUT")
	apiRouter.HandleFunc("/libraries/{libraryid}/objects/{id:[0-9a-f]{64}}", getObjectHandler).Methods("GET", "HEAD")
	apiRouter.HandleFunc("/libraries/{libraryid}/objects/{id:[0-9a-f]{64}}", putObjectHandler).Methods("PUT")
	apiRouter.HandleFunc("/libraries/{libraryid}/head", putHeadHandler).Methods("PUT")
	// The entries surface. One route, all methods: entriesHandler answers a
	// bad method with 405 and an Allow header, which mux would otherwise turn
	// into a 404 that reads as "wrong path".
	//
	// "All methods" now includes QUERY, which is load-bearing rather than
	// incidental: the range read is a QUERY on this route, and it works
	// because nothing here filters on method. A .Methods() list added to this
	// line would have to name it, and forgetting to would turn a working
	// endpoint into a 404 — see entries_ranges.go.
	apiRouter.HandleFunc("/libraries/{libraryid}/entries/{path:.*}", entriesHandler)

	return r
}

func RecoverWrapper(f func()) {
	defer func() {
		if err := recover(); err != nil {
			observability.Panic(context.Background(), "RecoverWrapper", err)
		}
	}()

	f()
}

// profilingAuthorized reports whether r carries the configured profiling
// password. The comparison is constant-time so response timing cannot reveal
// how much of a guess was correct. An empty configured password is never
// valid: option.go already fatals when profile_password is absent, but a
// present-and-empty value would otherwise leave pprof open to everyone.
func profilingAuthorized(r *http.Request) bool {
	if !option.EnableProfiling || option.ProfilePassword == "" {
		return false
	}
	password := r.URL.Query().Get("password")
	return subtle.ConstantTimeCompare([]byte(password), []byte(option.ProfilePassword)) == 1
}

type profileHandler struct {
	pHandler http.Handler
}

func (p *profileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !profilingAuthorized(r) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	p.pHandler.ServeHTTP(w, r)
}

type traceHandler struct {
}

func (p *traceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !profilingAuthorized(r) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	pprof.Trace(w, r)
}
