// Package silod is the Silo file server daemon.
package silod

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/metrics"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/tokenstore"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/dkam/silo/internal/observability"
	"github.com/dkam/silo/internal/xdg"
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
	option.LoadFileServerOptions(configFile)
	loadDatabase()
	repomgr.Init(siloPair.Read, siloPair.Write, dataDir)
	return nil
}

// DatabaseName is the single SQLite file every table lives in, relative to
// the data directory.
const DatabaseName = "silo.db"

func loadDatabase() {
	dbPath := filepath.Join(absDataDir, DatabaseName)

	var err error
	siloPair, err = dbutil.OpenSQLite(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	if err := dbutil.CreateSiloTables(siloPair.Write); err != nil {
		log.Fatalf("Failed to create tables: %v", err)
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
	fs.StringVar(&bindAddr, "b", "", "bind address (default: $SILO_HOST or 127.0.0.1)")
	fs.StringVar(&logFile, "l", "", "log file path (default: stdout)")
	fs.StringVar(&pidFilePath, "P", "", "pid file path")
	fs.BoolVar(&debugLog, "debug", false, "log every HTTP request (method, path, status, duration)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	if pidFilePath != "" {
		if err := writePidFile(pidFilePath); err != nil {
			log.Fatalf("Failed to write pid file %s: %v", pidFilePath, err)
		}
	}

	if err := resolvePaths(); err != nil {
		log.Fatalf("%v", err)
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
		if err := utils.Dup(int(logFp.Fd()), int(os.Stderr.Fd())); err != nil {
			log.Warnf("Failed to dup stderr to log file: %v", err)
		}
	} else {
		logToStdout = true
	}

	// After the log destination is settled, so the "reporting to" line lands
	// in the same place as everything else, and before the rest of startup, so
	// a failure to load config, open a database or create the admin is a
	// failure that gets reported rather than one that only exists in a log
	// nobody is reading.
	defer observability.Init("fileserver", option.Version)()

	if err := option.LoadJWTConfig(); err != nil {
		log.Fatalf("Failed to load JWT config: %v", err)
	}

	option.LoadFileServerOptions(configFile)
	// After the options, so the flag beats both SILO_HOST and the config file.
	// Same precedence as -d over SILO_DATA_DIR: what you typed on this command
	// line wins over what the environment happens to be carrying.
	if bindAddr != "" {
		option.Host = bindAddr
	}
	loadDatabase()

	level, err := log.ParseLevel(option.LogLevel)
	if err != nil {
		log.Info("use the default log level: info")
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(level)
	}

	repomgr.Init(siloPair.Read, siloPair.Write, dataDir)

	share.Init(siloPair.Read, option.GroupTableName, option.CloudMode)

	tokenstore.StartCleanup()
	account.Init(siloPair.Read, siloPair.Write)
	authmgr.Init(siloPair.Read, siloPair.Write)
	api.Init(siloPair.Read, siloPair.Write)
	api.StartLoginLimiterCleanup()
	apitokenstore.Init(siloPair.Read, siloPair.Write)
	apitokenstore.StartCleanup()

	// Create the admin user from the environment if it is set, and invent one
	// if it is not and there are no users at all. A server nobody can log in
	// to is not a useful server, and until now that was what `silo serve` gave
	// anyone who had not set the two variables before the first boot.
	adminEmail := os.Getenv("SILO_ADMIN_EMAIL")
	adminPassword := os.Getenv("SILO_ADMIN_PASSWORD")
	if adminEmail == "" {
		adminEmail = authmgr.DefaultAdminEmail
	}
	generated, err := authmgr.BootstrapAdmin(adminEmail, adminPassword)
	if err != nil {
		log.Fatalf("Failed to create admin user: %v", err)
	}
	if generated != "" {
		logGeneratedAdmin(adminEmail, generated)
	}

	metrics.Init()

	notif.Init()

	router := newHTTPRouter()

	httpServer = new(http.Server)
	httpServer.Addr = fmt.Sprintf("%s:%d", option.Host, option.Port)
	var handler http.Handler = router
	if debugLog {
		handler = middleware.DebugLogger(handler)
	}
	// Outermost, so a panic in any of the above is still reported and every
	// request is timed from the moment it arrives rather than from after the
	// prefix rewrite.
	handler = observability.Middleware(handler)
	httpServer.Handler = handler
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
	log.Printf("Silo server listening on http://%s:%d", option.Host, option.Port)
	warnIfExposedWithoutTLS()

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Errorf("File server exiting: %v", err)
		}
	}()

	<-shutdownDone
	return nil
}

// logGeneratedAdmin prints the credentials the server just invented for
// itself.
//
// This is the only time the password is ever legible: it is stored hashed, so
// nothing on the server can recover it and no later run will print it again.
// It goes out at warning level and over several lines on purpose — an operator
// scanning a first boot has to be able to find it, and the line that says
// "this will not be shown again" is the one that decides whether they write it
// down now or go looking for it later.
func logGeneratedAdmin(email, password string) {
	log.Warn("No users existed and no SILO_ADMIN_PASSWORD was set, so an admin account was created:")
	log.Warnf("    email:    %s", email)
	log.Warnf("    password: %s", password)
	log.Warn("This password is stored hashed and will not be shown again. Save it now.")
}

// warnIfExposedWithoutTLS says so when the server is reachable from off the
// machine over plaintext. Every credential Silo uses is a bearer token sent in
// a header, so anyone on the path can lift one and keep it.
//
// Silo speaks plaintext and only plaintext: TLS belongs to the reverse proxy
// in front of it. It is a warning rather than a refusal because the server
// cannot tell from here whether a proxy is there.
func warnIfExposedWithoutTLS() {
	ip := net.ParseIP(option.Host)
	if ip != nil && ip.IsLoopback() {
		return
	}
	log.Warnf("Listening on %s without TLS. Passwords and tokens will cross the "+
		"network in clear text — put a TLS reverse proxy in front, and set "+
		"SILO_TRUST_PROXY_HEADERS=true so rate limiting sees the real client.",
		option.Host)
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

	checkpointAndClose(siloPair)

	metrics.Stop()
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

	if err := utils.Dup(int(logFp.Fd()), int(os.Stderr.Fd())); err != nil {
		log.Warnf("Failed to dup stderr to log file: %v", err)
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
	if option.EnableNotification {
		r.HandleFunc("/notification", notif.Handler)
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
	r.HandleFunc("/api/silo/v1/server-info", api.ServerInfoHandler).Methods("GET")
	apiRouter := r.PathPrefix("/api/silo/v1").Subrouter()
	apiRouter.Use(middleware.RequireAuth)
	apiRouter.HandleFunc("/access-tokens", api.CreateAccessTokenHandler).Methods("POST")
	apiRouter.HandleFunc("/account/usage", api.AccountUsageHandler).Methods("GET")
	apiRouter.HandleFunc("/repos", api.ListReposHandler).Methods("GET")
	apiRouter.HandleFunc("/repos", api.CreateRepoHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}", api.DeleteRepoHandler).Methods("DELETE")
	apiRouter.HandleFunc("/repos/{repoid}", patchRepoHandler).Methods("PATCH")
	apiRouter.HandleFunc("/repos/{repoid}/changes", api.ChangesHandler).Methods("GET")
	// The chunk surface. "missing" cannot collide with a chunk id — the id
	// route only matches 64 hex characters — but it is listed first anyway,
	// because relying on a regex to keep two routes apart is the kind of thing
	// that stops being true when someone loosens the regex.
	apiRouter.HandleFunc("/repos/{repoid}/batch", batchHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}/blocks/missing", blocksMissingHandler).Methods("POST")
	// The id-addressed surface. A chunk id is sixty-four hex characters, so
	// the id and the route regex cannot collide — the width is the format, not
	// a convention. See objects.go.
	apiRouter.HandleFunc("/repos/{repoid}/blocks/{id:[0-9a-f]{64}}", getChunkHandler).Methods("GET", "HEAD")
	apiRouter.HandleFunc("/repos/{repoid}/blocks/{id:[0-9a-f]{64}}", putChunkHandler).Methods("PUT")
	apiRouter.HandleFunc("/repos/{repoid}/objects/{id:[0-9a-f]{64}}", getObjectHandler).Methods("GET", "HEAD")
	apiRouter.HandleFunc("/repos/{repoid}/objects/{id:[0-9a-f]{64}}", putObjectHandler).Methods("PUT")
	apiRouter.HandleFunc("/repos/{repoid}/head", putHeadHandler).Methods("PUT")
	// The entries surface. One route, all methods: entriesHandler answers a
	// bad method with 405 and an Allow header, which mux would otherwise turn
	// into a 404 that reads as "wrong path".
	apiRouter.HandleFunc("/repos/{repoid}/entries/{path:.*}", entriesHandler)
	apiRouter.HandleFunc("/repos/{repoid}/notify-token", api.CreateNotifyTokenHandler).Methods("POST")

	if option.HasRedisOptions {
		r.Use(metrics.MetricMiddleware)
	}
	return r
}

func handleProtocolVersion(rsp http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(rsp, "{\"version\": 2}")
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
