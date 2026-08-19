// Package silod is the Silo file server daemon.
package silod

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/keycache"
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
	"github.com/go-sql-driver/mysql"
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

var seafilePair, ccnetPair *dbutil.DBPair

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
			for _, name := range []string{"silo.conf", "seafile.conf"} {
				candidate := filepath.Join(xdgConf, name)
				if _, err := os.Stat(candidate); err == nil {
					configFile = candidate
					break
				}
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
	loadDatabases()
	repomgr.Init(seafilePair.Read, seafilePair.Write)
	return nil
}

func loadDatabases() {
	dbOpt, err := option.LoadDBOption(configFile)
	if err != nil {
		log.Fatalf("Failed to load database: %v", err)
	}

	dbutil.DBEngine = dbOpt.DBEngine
	if dbOpt.DBEngine == dbutil.EngineSQLite {
		loadSQLiteDatabases()
	} else {
		loadMySQLDatabases(dbOpt)
	}

	// Runs for both engines: CreateSeafileTables only executes on the SQLite
	// path, but a MySQL deployment with an externally-provisioned schema needs
	// the added columns just as much.
	if err := dbutil.MigrateSeafileTables(seafilePair.Write, option.APITokenTTL); err != nil {
		log.Fatalf("Failed to migrate seafile database: %v", err)
	}
}

func loadSQLiteDatabases() {
	ccnetPath := filepath.Join(absDataDir, "ccnet.db")
	seafilePath := filepath.Join(absDataDir, "seafile.db")

	var err error
	ccnetPair, err = dbutil.OpenSQLite(ccnetPath)
	if err != nil {
		log.Fatalf("Failed to open ccnet database: %v", err)
	}

	if err := dbutil.CreateCcnetTables(ccnetPair.Write); err != nil {
		log.Fatalf("Failed to create ccnet tables: %v", err)
	}

	seafilePair, err = dbutil.OpenSQLite(seafilePath)
	if err != nil {
		log.Fatalf("Failed to open seafile database: %v", err)
	}

	if err := dbutil.CreateSeafileTables(seafilePair.Write); err != nil {
		log.Fatalf("Failed to create seafile tables: %v", err)
	}

	log.Info("Using SQLite databases")
}

func loadMySQLDatabases(dbOpt *option.DBOption) {
	ccnetDSN := buildMySQLDSN(dbOpt, dbOpt.CcnetDbName)
	seafileDSN := buildMySQLDSN(dbOpt, dbOpt.SeafileDbName)

	var err error
	ccnetPair, err = dbutil.OpenMySQL(ccnetDSN)
	if err != nil {
		log.Fatalf("Failed to open ccnet database: %v", err)
	}

	seafilePair, err = dbutil.OpenMySQL(seafileDSN)
	if err != nil {
		log.Fatalf("Failed to open seafile database: %v", err)
	}

	log.Info("Using MySQL databases")
}

func buildMySQLDSN(dbOpt *option.DBOption, dbName string) string {
	timeout := "&readTimeout=60s" + "&writeTimeout=60s"
	var dsn string
	if dbOpt.UseTLS && dbOpt.SkipVerify {
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=skip-verify%s", dbOpt.User, dbOpt.Password, dbOpt.Host, dbOpt.Port, dbName, timeout)
	} else if dbOpt.UseTLS && !dbOpt.SkipVerify {
		registerCA(dbOpt.CaPath)
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=custom%s", dbOpt.User, dbOpt.Password, dbOpt.Host, dbOpt.Port, dbName, timeout)
	} else {
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=%t%s", dbOpt.User, dbOpt.Password, dbOpt.Host, dbOpt.Port, dbName, dbOpt.UseTLS, timeout)
	}
	if dbOpt.Charset != "" {
		dsn = fmt.Sprintf("%s&charset=%s", dsn, dbOpt.Charset)
	}
	return dsn
}

// registerCA registers CA to verify server cert.
func registerCA(capath string) {
	rootCertPool := x509.NewCertPool()
	pem, err := os.ReadFile(capath)
	if err != nil {
		log.Fatal(err)
	}
	if ok := rootCertPool.AppendCertsFromPEM(pem); !ok {
		log.Fatal("Failed to append PEM.")
	}
	if err := mysql.RegisterTLSConfig("custom", &tls.Config{
		RootCAs: rootCertPool,
	}); err != nil {
		log.Fatalf("Failed to register TLS config: %v", err)
	}
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
	loadDatabases()

	level, err := log.ParseLevel(option.LogLevel)
	if err != nil {
		log.Info("use the default log level: info")
		log.SetLevel(log.InfoLevel)
	} else {
		log.SetLevel(level)
	}

	repomgr.Init(seafilePair.Read, seafilePair.Write)

	// Drop cached authorisations as soon as the rows behind them go away,
	// rather than at the next cache expiry. Registered here because repomgr
	// sits below this package and cannot call into it.
	repomgr.OnRepoDeleted = invalidateRepoAuth
	repomgr.OnTokensRevoked = invalidateUserAuth

	// First arg is the legacy "central config path"; it's threaded into
	// objstore.New but never used there. Passing "" keeps the signatures
	// untouched until a wider cleanup removes the parameter entirely.
	fsmgr.Init("", dataDir, option.FsCacheLimit)

	blockmgr.Init("", dataDir)

	commitmgr.Init("", dataDir)

	share.Init(ccnetPair.Read, seafilePair.Read, option.GroupTableName, option.CloudMode)

	tokenstore.StartCleanup()
	keycache.StartReaper()
	authmgr.Init(ccnetPair.Read, ccnetPair.Write)
	api.Init(seafilePair.Read, seafilePair.Write)
	api.StartLoginLimiterCleanup()
	apitokenstore.Init(seafilePair.Read, seafilePair.Write)
	apitokenstore.StartCleanup()

	// Create the admin user from the environment if it is set, and invent one
	// if it is not and there are no users at all. A server nobody can log in
	// to is not a useful server, and until now that was what `silo serve` gave
	// anyone who had not set the two variables before the first boot.
	adminEmail := option.EnvWithFallback("SILO_ADMIN_EMAIL", "SEAFILE_ADMIN_EMAIL")
	adminPassword := option.EnvWithFallback("SILO_ADMIN_PASSWORD", "SEAFILE_ADMIN_PASSWORD")
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

	fileopInit()

	syncAPIInit()

	sizeSchedulerInit()

	virtualRepoInit()

	initUpload()

	metrics.Init()

	notif.Init()

	router := newHTTPRouter()

	httpServer = new(http.Server)
	httpServer.Addr = fmt.Sprintf("%s:%d", option.Host, option.Port)
	var handler = middleware.StripSeafhttpPrefix(router)
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

	tlsCert, tlsKey := option.TLSCertFile, option.TLSKeyFile
	useTLS := tlsCert != "" && tlsKey != ""
	scheme := "http"
	if useTLS {
		scheme = "https"
		// Pinned rather than left to the default so that the floor is a
		// property of this server and not of whichever Go version built it.
		// 1.2 rather than 1.3 because Silo exists to keep existing Seafile
		// clients working, and their TLS comes from whatever OpenSSL the
		// platform shipped.
		httpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
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
	log.Printf("Silo server listening on %s://%s:%d", scheme, option.Host, option.Port)
	warnIfExposedWithoutTLS(useTLS)

	go func() {
		var err error
		if useTLS {
			err = httpServer.ServeTLS(ln, tlsCert, tlsKey)
		} else {
			err = httpServer.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
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
// a header — sync tokens, API tokens, JWTs — so anyone on the path can lift
// one and keep it. It is a warning rather than a refusal because terminating
// TLS at a reverse proxy is the normal deployment, and the server cannot tell
// from here whether one is in front of it.
func warnIfExposedWithoutTLS(useTLS bool) {
	if useTLS {
		return
	}
	ip := net.ParseIP(option.Host)
	if ip != nil && ip.IsLoopback() {
		return
	}
	log.Warnf("Listening on %s without TLS. Passwords and tokens will cross the "+
		"network in clear text — put a TLS reverse proxy in front (and set "+
		"SILO_TRUST_PROXY_HEADERS=true), or set SILO_TLS_CERT and SILO_TLS_KEY.",
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

	// Checkpoint both DBs in parallel — they're independent and a large WAL
	// can take a noticeable fraction of a second to flush.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); checkpointAndClose(seafilePair, "seafile") }()
	go func() { defer wg.Done(); checkpointAndClose(ccnetPair, "ccnet") }()
	wg.Wait()

	metrics.Stop()
	if err := removePidfile(pidFilePath); err != nil {
		log.Warnf("Failed to remove pid file: %v", err)
	}

	log.Info("shutdown complete")
	close(shutdownDone)
}

func checkpointAndClose(pair *dbutil.DBPair, name string) {
	if pair == nil {
		return
	}
	if option.DBType == dbutil.EngineSQLite {
		if _, err := pair.Write.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			log.Warnf("%s WAL checkpoint failed: %v", name, err)
		}
	}
	if err := pair.Close(); err != nil {
		log.Warnf("%s DB close failed: %v", name, err)
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
	r.HandleFunc("/protocol-version{slash:\\/?}", handleProtocolVersion)
	r.Handle("/files/{.*}/{.*}", appHandler(accessCB))
	r.Handle("/blks/{.*}/{.*}", appHandler(accessBlksCB))
	r.Handle("/zip/{.*}", appHandler(accessZipCB))
	r.Handle("/upload-api/{.*}", appHandler(uploadAPICB))
	r.Handle("/upload-aj/{.*}", appHandler(uploadAjaxCB))
	r.Handle("/update-api/{.*}", appHandler(updateAPICB))
	r.Handle("/update-aj/{.*}", appHandler(updateAjaxCB))
	r.Handle("/upload-blks-api/{.*}", appHandler(uploadBlksAPICB))
	r.Handle("/upload-raw-blks-api/{.*}", appHandler(uploadRawBlksAPICB))

	// The share-link routes (/f/, /u/, /d/) and the web file-access route
	// (/repos/{id}/files/{path}) were removed: every one of them authorized
	// by POSTing to Seahub, which a standalone Silo deploy does not run, so
	// they could only ever fail. Reinstating them means implementing the
	// authorization against Silo's own share store, not restoring these.

	// file syncing api
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/permission-check{slash:\\/?}",
		appHandler(permissionCheckCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/commit/HEAD{slash:\\/?}",
		appHandler(headCommitOperCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/commit/{id:[\\da-z]{40}}",
		appHandler(commitOperCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/block/{id:[\\da-z]{40}}",
		appHandler(blockOperCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/fs-id-list{slash:\\/?}",
		appHandler(getFsObjIDCB))
	r.Handle("/repo/head-commits-multi{slash:\\/?}",
		appHandler(headCommitsMultiCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/pack-fs{slash:\\/?}",
		appHandler(packFSCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/check-fs{slash:\\/?}",
		appHandler(checkFSCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/check-blocks{slash:\\/?}",
		appHandler(checkBlockCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/recv-fs{slash:\\/?}",
		appHandler(recvFSCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/quota-check{slash:\\/?}",
		appHandler(getCheckQuotaCB))
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/jwt-token{slash:\\/?}",
		appHandler(getJWTTokenCB))

	// seadrive api
	r.Handle("/repo/{repoid:[\\da-z]{8}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{4}-[\\da-z]{12}}/block-map/{id:[\\da-z]{40}}",
		appHandler(getBlockMapCB))
	r.Handle("/accessible-repos{slash:\\/?}", appHandler(getAccessibleRepoListCB))

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
	apiRouter.HandleFunc("/repos", api.ListReposHandler).Methods("GET")
	apiRouter.HandleFunc("/repos", api.CreateRepoHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}", api.DeleteRepoHandler).Methods("DELETE")
	apiRouter.HandleFunc("/repos/{repoid}", patchRepoHandler).Methods("PATCH")
	apiRouter.HandleFunc("/repos/{repoid}/changes", api.ChangesHandler).Methods("GET")
	// The block surface. "missing" cannot collide with a block id — the id
	// route only matches 40 hex characters — but it is listed first anyway,
	// because relying on a regex to keep two routes apart is the kind of thing
	// that stops being true when someone loosens the regex.
	apiRouter.HandleFunc("/repos/{repoid}/batch", batchHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}/blocks/missing", blocksMissingHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}/blocks/{id:[0-9a-f]{40}}", putBlockHandler).Methods("PUT")
	// The entries surface. One route, all methods: entriesHandler answers a
	// bad method with 405 and an Allow header, which mux would otherwise turn
	// into a 404 that reads as "wrong path".
	apiRouter.HandleFunc("/repos/{repoid}/entries/{path:.*}", entriesHandler)
	apiRouter.HandleFunc("/repos/{repoid}/sync-token", api.CreateRepoSyncTokenHandler).Methods("POST")
	apiRouter.HandleFunc("/repos/{repoid}/notify-token", api.CreateNotifyTokenHandler).Methods("POST")

	// SeaDrive compatibility routes (/api2/)
	// These use Seahub/DRF-style "Authorization: Token <token>" auth, not Bearer JWT.
	r.HandleFunc("/api2/auth-token/", api.SeaDriveAuthTokenHandler).Methods("POST")
	api2Router := r.PathPrefix("/api2").Subrouter()
	api2Router.Use(middleware.RequireAPIToken)
	api2Router.HandleFunc("/auth/ping/", api.SeaDriveAuthPingHandler).Methods("GET")
	api2Router.HandleFunc("/auth/logout/", api.SeaDriveLogoutHandler).Methods("POST")
	api2Router.HandleFunc("/account/info/", api.SeaDriveAccountInfoHandler).Methods("GET")
	api2Router.HandleFunc("/server-info/", api.SeaDriveServerInfoHandler).Methods("GET")
	api2Router.HandleFunc("/repos/", api.SeaDriveReposHandler).Methods("GET")
	api2Router.HandleFunc("/repos/", api.SeaDriveCreateRepoHandler).Methods("POST")
	api2Router.HandleFunc("/repos/{repoid}/", renameRepoHandler).Methods("POST").Queries("op", "rename")
	api2Router.HandleFunc("/repos/{repoid}/download-info/", api.SeaDriveDownloadInfoHandler).Methods("GET")
	api2Router.HandleFunc("/repos/{repoid}/repo-tokens/", api.CreateRepoSyncTokenHandler).Methods("POST")

	// SeaDrive also uses /api/v2.1/ for some operations (delete, rename).
	api21Router := r.PathPrefix("/api/v2.1").Subrouter()
	api21Router.Use(middleware.RequireAPIToken)
	api21Router.HandleFunc("/repos/{repoid}/", renameRepoHandler).Methods("POST").Queries("op", "rename")
	api21Router.HandleFunc("/repos/{repoid}/", api.DeleteRepoHandler).Methods("DELETE")

	if option.HasRedisOptions {
		r.Use(metrics.MetricMiddleware)
	}
	return r
}

func handleProtocolVersion(rsp http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(rsp, "{\"version\": 2}")
}

type appError struct {
	Error   error
	Message string
	Code    int
}

type appHandler func(http.ResponseWriter, *http.Request) *appError

func (fn appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e := fn(w, r); e != nil {
		if e.Error != nil && e.Code == http.StatusInternalServerError {
			// WithContext so the report carries the request that caused it —
			// the URL alone doesn't say which account or which client.
			log.WithContext(r.Context()).WithError(e.Error).
				Errorf("path %s internal server error: %v\n", r.URL.Path, e.Error)
		}
		http.Error(w, e.Message, e.Code)
	}
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
