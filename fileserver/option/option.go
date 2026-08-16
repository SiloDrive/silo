package option

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
)

// InfiniteQuota indicates that the quota is unlimited.
const InfiniteQuota = -2

// MinAPITokenTTL is the shortest API token lifetime an operator may configure.
// It is a floor on operator input, not on the code: dbutil separately refuses a
// non-positive TTL, which catches the different failure of a caller reaching the
// migration before options are loaded.
//
// An hour is already far shorter than any real deployment wants for a sliding
// credential; anything below it is a typo rather than an intent.
const MinAPITokenTTL = time.Hour

// Storage unit.
const (
	KB = 1000
	MB = 1000000
	GB = 1000000000
	TB = 1000000000000
)

var (
	// fileserver options
	Host                   string
	Port                   uint32
	MaxUploadSize          uint64
	FsIdListRequestTimeout int64
	// Block size for indexing uploaded files
	FixedBlockSize uint64
	// Maximum number of goroutines to index uploaded files
	MaxIndexingThreads uint32
	WebTokenExpireTime uint32
	// File mode for temp files
	ClusterSharedTempFileMode uint32
	WindowsEncoding           string
	SkipBlockHash             bool
	FsCacheLimit              int64
	VerifyClientBlocks        bool
	MaxIndexingFiles          uint32

	// general options
	CloudMode bool

	// notification server
	EnableNotification bool

	// GROUP options
	GroupTableName string

	// quota options
	DefaultQuota int64

	// redis options
	HasRedisOptions bool
	RedisHost       string
	RedisPasswd     string
	RedisPort       uint32
	RedisExpiry     uint32
	RedisMaxConn    uint32
	RedisTimeout    time.Duration

	// Profile password
	ProfilePassword string
	EnableProfiling bool

	// Go log level
	LogLevel string

	// DB default timeout
	DBOpTimeout time.Duration

	// AuthCacheTTL bounds how long validateToken and checkPermission may
	// answer from memory before consulting the database again. It is the
	// window in which a revoked token or a deleted library still works on a
	// running server.
	//
	// Changes this process makes itself — a library deleted through the
	// management API — purge the caches immediately, so the TTL only bounds
	// what it cannot see: `silo token revoke` running as a separate process,
	// and edits made directly to the database.
	//
	// Five minutes keeps effectively all of the benefit. The caches exist to
	// keep a database round trip out of the path of every block request, and
	// an actively syncing client makes far more than one request per five
	// minutes. Set SILO_AUTH_CACHE_TTL=0 to check the database every time.
	AuthCacheTTL time.Duration

	// SyncObjectWrites fsyncs every commit, fs and block object before it is
	// published, and fsyncs the directory entry after. On by default: the
	// branch head lives in SQLite, which fsyncs its own WAL, so without this
	// a power cut can leave a durable head pointing at objects that never
	// reached the platter. That damage does not heal — the client believes
	// those blocks are uploaded and never sends them again.
	//
	// Set SILO_SYNC_OBJECT_WRITES=false only where the storage is already
	// crash-safe by other means, or where losing the last few seconds of
	// writes is genuinely acceptable.
	//
	// Initialised here rather than only in initDefaultOptions so that the
	// durable behaviour is what you get without loading options at all —
	// the bool zero value would otherwise turn fsync off for any caller
	// that reaches the object store first.
	SyncObjectWrites = true

	// APITokenTTL bounds how long an /api2/ API token stays valid without
	// being used. The expiry slides on use, so an actively syncing client is
	// never logged out; only an idle — or leaked and unused — token ages out.
	//
	// Configurable because the failure mode on the client side is not fully
	// known: a client that does not re-authenticate on 401 would stop working
	// at the TTL, and an operator who hits that needs a way to raise it
	// without a rebuild. Sync tokens (RepoUserToken) deliberately have no
	// equivalent — Seafile clients persist those and treat them as durable.
	APITokenTTL time.Duration

	// TLSCertFile and TLSKeyFile, when both are set, make the server speak
	// HTTPS directly. Left empty, it speaks plaintext and expects a TLS
	// reverse proxy in front of it — every credential Silo uses is a bearer
	// token in a header, so plaintext on an exposed address gives them away.
	TLSCertFile string
	TLSKeyFile  string

	// LoginRateLimit throttles failed password attempts per client address
	// and per account. On by default: both login endpoints run a PBKDF2
	// verification with nothing in front of it, so without this an attacker
	// can guess online as fast as the server can hash, and every success
	// mints a durable token.
	//
	// SILO_LOGIN_RATE_LIMIT=false turns it off, for a deployment that
	// throttles at the proxy instead.
	LoginRateLimit = true

	// TrustProxyHeaders decides whether X-Forwarded-For and X-Real-Ip are
	// believed when attributing a request to a client address.
	//
	// Off by default, because the headers are trivially forged and the rate
	// limiter counts by address: believing them would hand an attacker a
	// fresh identity per request. Behind a reverse proxy it must be turned
	// on, or every client shares the proxy's bucket and one attacker
	// throttles everyone.
	TrustProxyHeaders bool

	// database — use dbutil.DBEngine for portable SQL helpers
	DBType string

	// seahub
	SeahubURL     string
	JWTPrivateKey string

	// metric
	NodeName string
)

type DBOption struct {
	User          string
	Password      string
	Host          string
	Port          int
	CcnetDbName   string
	SeafileDbName string
	CaPath        string
	UseTLS        bool
	SkipVerify    bool
	Charset       string
	DBEngine      string
}

// EnvWithFallback returns the first non-empty value from the named env vars.
func EnvWithFallback(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func initDefaultOptions() {
	// Loopback by default. Silo speaks plaintext unless given a certificate,
	// and every credential it uses is a bearer token in a header, so a
	// default that publishes the port to the whole network is a default that
	// gives those tokens to anyone on it. Exposing the server is a decision
	// to make deliberately, with SILO_HOST or the config file — the Docker
	// image sets SILO_HOST=0.0.0.0 itself, since a container that binds
	// loopback cannot be reached through a published port at all.
	Host = "127.0.0.1"
	Port = 8082
	FixedBlockSize = 1 << 23
	MaxIndexingThreads = 1
	WebTokenExpireTime = 7200
	ClusterSharedTempFileMode = 0600
	DefaultQuota = InfiniteQuota
	FsCacheLimit = 4 << 30
	VerifyClientBlocks = true
	FsIdListRequestTimeout = -1
	DBOpTimeout = 60 * time.Second
	RedisHost = "127.0.0.1"
	RedisPort = 6379
	RedisExpiry = 24 * 3600
	RedisMaxConn = 100
	RedisTimeout = 1 * time.Second
	MaxIndexingFiles = 10
	APITokenTTL = 30 * 24 * time.Hour
	SyncObjectWrites = true
	AuthCacheTTL = 5 * time.Minute
}

// LoadFileServerOptions loads seafile.conf from the given path. An empty
// path or a missing file is fine — Silo then runs entirely on compiled
// defaults plus environment variable overrides.
func LoadFileServerOptions(configFile string) {
	initDefaultOptions()

	opts := ini.LoadOptions{}
	opts.SpaceBeforeInlineComment = true

	var config *ini.File
	if configFile != "" {
		if _, err := os.Stat(configFile); err == nil {
			loaded, err := ini.LoadSources(opts, configFile)
			if err != nil {
				log.Fatalf("Failed to load %s: %v", configFile, err)
			}
			config = loaded
		}
	}
	if config == nil {
		// No config file — run on defaults + env vars.
		config = ini.Empty(opts)
	}
	CloudMode = false
	if section, err := config.GetSection("general"); err == nil {
		if key, err := section.GetKey("cloud_mode"); err == nil {
			CloudMode, _ = key.Bool()
		}
	}

	// Notification server: silo runs it in-process at /notification.
	// Enabled by default; set SILO_ENABLE_NOTIFICATIONS=false to disable.
	EnableNotification = true
	if v := EnvWithFallback("SILO_ENABLE_NOTIFICATIONS", "ENABLE_NOTIFICATION_SERVER"); v == "false" || v == "0" {
		EnableNotification = false
	}

	// Accepts a Go duration ("5m", "30s") or "0" to disable the auth caches
	// entirely. Unlike the token TTL, a wrong value here is recoverable by
	// fixing it and restarting, so it is clamped rather than rejected: a
	// negative duration means the same thing as zero.
	if v := os.Getenv("SILO_AUTH_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			log.Warnf("Ignoring unparseable SILO_AUTH_CACHE_TTL %q, using %s", v, AuthCacheTTL)
		case d <= 0:
			log.Info("SILO_AUTH_CACHE_TTL is zero: every request will re-check the database.")
			AuthCacheTTL = 0
		default:
			AuthCacheTTL = d
		}
	}

	LoginRateLimit = true
	if v := os.Getenv("SILO_LOGIN_RATE_LIMIT"); v == "false" || v == "0" {
		LoginRateLimit = false
		log.Warn("SILO_LOGIN_RATE_LIMIT is off: password guessing against the login " +
			"endpoints is unthrottled.")
	}

	TrustProxyHeaders = false
	if v := os.Getenv("SILO_TRUST_PROXY_HEADERS"); v == "true" || v == "1" {
		TrustProxyHeaders = true
	}

	TLSCertFile = os.Getenv("SILO_TLS_CERT")
	TLSKeyFile = os.Getenv("SILO_TLS_KEY")
	if (TLSCertFile == "") != (TLSKeyFile == "") {
		// One without the other silently means no TLS, which is exactly the
		// mistake worth failing loudly on.
		log.Fatal("SILO_TLS_CERT and SILO_TLS_KEY must be set together.")
	}

	// Durability of object writes. Opt-out only — an operator has to say so
	// explicitly, because the failure it protects against is silent and
	// unrecoverable rather than merely inconvenient.
	if v := os.Getenv("SILO_SYNC_OBJECT_WRITES"); v == "false" || v == "0" {
		SyncObjectWrites = false
		log.Warn("SILO_SYNC_OBJECT_WRITES is off: objects are not fsynced, " +
			"so a crash or power loss can leave repositories permanently corrupt.")
	}

	if section, err := config.GetSection("httpserver"); err == nil {
		parseFileServerSection(section)
	}
	if section, err := config.GetSection("fileserver"); err == nil {
		parseFileServerSection(section)
	}

	// Environment overrides for bind address.
	if envHost := EnvWithFallback("SILO_HOST", "SEAFILE_FILESERVER_HOST"); envHost != "" {
		Host = envHost
	}
	if envPort := EnvWithFallback("SILO_PORT", "SEAFILE_FILESERVER_PORT"); envPort != "" {
		if port, err := strconv.ParseUint(envPort, 10, 32); err == nil {
			Port = uint32(port)
		}
	}

	if section, err := config.GetSection("quota"); err == nil {
		if key, err := section.GetKey("default"); err == nil {
			quotaStr := key.String()
			DefaultQuota = parseQuota(quotaStr)
		}
	}

	loadCacheOptionFromEnv()

	GroupTableName = os.Getenv("SEAFILE_MYSQL_DB_GROUP_TABLE_NAME")
	if GroupTableName == "" {
		GroupTableName = "Group"
	}

	NodeName = os.Getenv("NODE_NAME")
	if NodeName == "" {
		NodeName = "default"
	}

	if lvl := os.Getenv("SILO_LOG_LEVEL"); lvl != "" {
		LogLevel = lvl
	}

	// Accepts a Go duration ("720h", "30m"). An unparseable value keeps the
	// default rather than disabling expiry, so a typo cannot silently turn API
	// tokens back into permanent credentials.
	if v := os.Getenv("SILO_API_TOKEN_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil:
			log.Warnf("Ignoring unparseable SILO_API_TOKEN_TTL %q, using %s", v, APITokenTTL)
		case d < MinAPITokenTTL:
			// Rejected rather than clamped, because the difference between a
			// deliberate short TTL and a typo is not knowable here and the
			// consequence of guessing wrong is not recoverable. The migration
			// stamps every pre-existing token with expires_at = now + TTL, and
			// backfills only rows where expires_at IS NULL — so once a too-short
			// TTL has stamped them, correcting the variable and restarting does
			// not undo it. Every token stays expired, with nothing in the logs
			// connecting the symptom to the cause.
			//
			// The realistic typo this catches is "30m" for an intended "30 days",
			// which is 720h.
			log.Warnf("Ignoring SILO_API_TOKEN_TTL %q: below the %s minimum. Using %s. "+
				"A shorter value would expire existing tokens irrecoverably.",
				v, MinAPITokenTTL, APITokenTTL)
		default:
			APITokenTTL = d
		}
	}
}

func parseFileServerSection(section *ini.Section) {
	if key, err := section.GetKey("host"); err == nil {
		Host = key.String()
	}
	if key, err := section.GetKey("port"); err == nil {
		port, err := key.Uint()
		if err == nil {
			Port = uint32(port)
		}
	}
	if key, err := section.GetKey("max_upload_size"); err == nil {
		size, err := key.Uint()
		if err == nil {
			MaxUploadSize = uint64(size) * 1000000
		}
	}
	if key, err := section.GetKey("max_indexing_threads"); err == nil {
		threads, err := key.Uint()
		if err == nil {
			MaxIndexingThreads = uint32(threads)
		}
	}
	if key, err := section.GetKey("fixed_block_size"); err == nil {
		blkSize, err := key.Uint64()
		if err == nil {
			FixedBlockSize = blkSize * (1 << 20)
		}
	}
	if key, err := section.GetKey("web_token_expire_time"); err == nil {
		expire, err := key.Uint()
		if err == nil {
			WebTokenExpireTime = uint32(expire)
		}
	}
	if key, err := section.GetKey("cluster_shared_temp_file_mode"); err == nil {
		fileMode, err := key.Uint()
		if err == nil {
			ClusterSharedTempFileMode = uint32(fileMode)
		}
	}
	if key, err := section.GetKey("enable_profiling"); err == nil {
		EnableProfiling, _ = key.Bool()
	}
	if EnableProfiling {
		if key, err := section.GetKey("profile_password"); err == nil {
			ProfilePassword = key.String()
		} else {
			log.Fatal("password of profiling must be specified.")
		}
	}
	if key, err := section.GetKey("go_log_level"); err == nil {
		LogLevel = key.String()
	}
	if key, err := section.GetKey("fs_cache_limit"); err == nil {
		fsCacheLimit, err := key.Int64()
		if err == nil {
			FsCacheLimit = fsCacheLimit * 1024 * 1024
		}
	}
	// The ratio of physical memory consumption and fs objects is about 4:1,
	// and this part of memory is generally not subject to GC. So the value is
	// divided by 4.
	FsCacheLimit = FsCacheLimit / 4
	if key, err := section.GetKey("fs_id_list_request_timeout"); err == nil {
		fsIdListRequestTimeout, err := key.Int64()
		if err == nil {
			FsIdListRequestTimeout = fsIdListRequestTimeout
		}
	}
	if key, err := section.GetKey("verify_client_blocks_after_sync"); err == nil {
		VerifyClientBlocks, _ = key.Bool()
	}
	if key, err := section.GetKey("max_indexing_files"); err == nil {
		threads, err := key.Uint()
		if err == nil && threads > 0 {
			MaxIndexingFiles = uint32(threads)
		}
	}
}

func parseQuota(quotaStr string) int64 {
	var quota int64
	var multiplier int64 = GB
	if end := strings.Index(quotaStr, "kb"); end > 0 {
		multiplier = KB
		quotaInt, err := strconv.ParseInt(quotaStr[:end], 10, 0)
		if err != nil {
			return InfiniteQuota
		}
		quota = quotaInt * multiplier
	} else if end := strings.Index(quotaStr, "mb"); end > 0 {
		multiplier = MB
		quotaInt, err := strconv.ParseInt(quotaStr[:end], 10, 0)
		if err != nil {
			return InfiniteQuota
		}
		quota = quotaInt * multiplier
	} else if end := strings.Index(quotaStr, "gb"); end > 0 {
		multiplier = GB
		quotaInt, err := strconv.ParseInt(quotaStr[:end], 10, 0)
		if err != nil {
			return InfiniteQuota
		}
		quota = quotaInt * multiplier
	} else if end := strings.Index(quotaStr, "tb"); end > 0 {
		multiplier = TB
		quotaInt, err := strconv.ParseInt(quotaStr[:end], 10, 0)
		if err != nil {
			return InfiniteQuota
		}
		quota = quotaInt * multiplier
	} else {
		quotaInt, err := strconv.ParseInt(quotaStr, 10, 0)
		if err != nil {
			return InfiniteQuota
		}
		quota = quotaInt * multiplier
	}

	return quota
}

func loadCacheOptionFromEnv() {
	cacheProvider := os.Getenv("CACHE_PROVIDER")
	if cacheProvider != "redis" {
		return
	}

	HasRedisOptions = true

	redisHost := os.Getenv("REDIS_HOST")
	if redisHost != "" {
		RedisHost = redisHost
	}
	redisPort := os.Getenv("REDIS_PORT")
	if redisPort != "" {
		port, err := strconv.ParseUint(redisPort, 10, 32)
		if err == nil {
			RedisPort = uint32(port)
		}
	}
	redisPasswd := os.Getenv("REDIS_PASSWORD")
	if redisPasswd != "" {
		RedisPasswd = redisPasswd
	}
	redisMaxConn := os.Getenv("REDIS_MAX_CONNECTIONS")
	if redisMaxConn != "" {
		maxConn, err := strconv.ParseUint(redisMaxConn, 10, 32)
		if err == nil {
			RedisMaxConn = uint32(maxConn)
		}
	}
	redisExpiry := os.Getenv("REDIS_EXPIRY")
	if redisExpiry != "" {
		expiry, err := strconv.ParseUint(redisExpiry, 10, 32)
		if err == nil {
			RedisExpiry = uint32(expiry)
		}
	}
}

func LoadJWTConfig() error {
	JWTPrivateKey = EnvWithFallback("SILO_JWT_SECRET", "JWT_PRIVATE_KEY")
	if JWTPrivateKey == "" {
		// Auto-generate a key. Tokens won't survive server restarts,
		// which is fine for a single-server deployment.
		buf := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return fmt.Errorf("failed to generate JWT key: %v", err)
		}
		JWTPrivateKey = hex.EncodeToString(buf)
		log.Info("SILO_JWT_SECRET not set, generated ephemeral key")
	}

	// SeahubURL now has exactly one caller left: postGetNickName, which looks
	// up a display name for merge conflict messages. The share-link and web
	// file-access paths that also used it were removed, since Silo runs no
	// Seahub and they could only fail. The remaining call degrades quietly —
	// postGetNickName falls back to the raw modifier string on any error.
	siteRoot := os.Getenv("SITE_ROOT")
	if siteRoot != "" {
		SeahubURL = fmt.Sprintf("http://127.0.0.1:8000%sapi/v2.1/internal", siteRoot)
	} else {
		SeahubURL = "http://127.0.0.1:8000/api/v2.1/internal"
	}

	return nil
}

func LoadDBOption(configFile string) (*DBOption, error) {
	dbOpt, err := loadDBOptionFromFile(configFile)
	if err != nil {
		log.Warnf("failed to load database config: %v", err)
		dbOpt = &DBOption{DBEngine: "sqlite"}
	}

	// Check env override for DB type
	if dbType := os.Getenv("SEAFILE_DB_TYPE"); dbType != "" {
		dbOpt.DBEngine = dbType
	}

	if dbOpt.DBEngine == "sqlite" {
		// SQLite needs no host/user/password
		DBType = "sqlite"
		return dbOpt, nil
	}

	dbOpt = loadDBOptionFromEnv(dbOpt)

	if dbOpt.Host == "" {
		return nil, fmt.Errorf("no database host in seafile.conf")
	}
	if dbOpt.User == "" {
		return nil, fmt.Errorf("no database user in seafile.conf")
	}
	if dbOpt.Password == "" {
		return nil, fmt.Errorf("no database password in seafile.conf")
	}

	DBType = dbOpt.DBEngine

	return dbOpt, nil
}

func loadDBOptionFromFile(configFile string) (*DBOption, error) {
	dbOpt := new(DBOption)
	// Default to SQLite — Silo ships with an embedded store and the
	// [database] section only needs to exist when the operator wants MySQL.
	dbOpt.DBEngine = "sqlite"

	if configFile == "" {
		return dbOpt, nil
	}
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		return dbOpt, nil
	}
	opts := ini.LoadOptions{}
	opts.SpaceBeforeInlineComment = true
	config, err := ini.LoadSources(opts, configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load %s: %v", configFile, err)
	}

	section, err := config.GetSection("database")
	if err != nil {
		return dbOpt, nil
	}

	dbEngine := "sqlite"
	key, err := section.GetKey("type")
	if err == nil {
		dbEngine = key.String()
	}
	if dbEngine != "mysql" && dbEngine != "sqlite" {
		return nil, fmt.Errorf("unsupported database %s", dbEngine)
	}
	dbOpt.DBEngine = dbEngine
	if key, err = section.GetKey("host"); err == nil {
		dbOpt.Host = key.String()
	}
	// user is required.
	if key, err = section.GetKey("user"); err == nil {
		dbOpt.User = key.String()
	}

	if key, err = section.GetKey("password"); err == nil {
		dbOpt.Password = key.String()
	}

	if key, err = section.GetKey("db_name"); err == nil {
		dbOpt.SeafileDbName = key.String()
	}
	port := 3306
	if key, err = section.GetKey("port"); err == nil {
		port, _ = key.Int()
	}
	dbOpt.Port = port
	useTLS := false
	if key, err = section.GetKey("use_ssl"); err == nil {
		useTLS, _ = key.Bool()
	}
	dbOpt.UseTLS = useTLS
	skipVerify := false
	if key, err = section.GetKey("skip_verify"); err == nil {
		skipVerify, _ = key.Bool()
	}
	dbOpt.SkipVerify = skipVerify
	if key, err = section.GetKey("ca_path"); err == nil {
		dbOpt.CaPath = key.String()
	}
	if key, err = section.GetKey("connection_charset"); err == nil {
		dbOpt.Charset = key.String()
	}

	return dbOpt, nil
}

func loadDBOptionFromEnv(dbOpt *DBOption) *DBOption {
	user := os.Getenv("SEAFILE_MYSQL_DB_USER")
	password := os.Getenv("SEAFILE_MYSQL_DB_PASSWORD")
	host := os.Getenv("SEAFILE_MYSQL_DB_HOST")
	portStr := os.Getenv("SEAFILE_MYSQL_DB_PORT")
	ccnetDbName := os.Getenv("SEAFILE_MYSQL_DB_CCNET_DB_NAME")
	seafileDbName := os.Getenv("SEAFILE_MYSQL_DB_SEAFILE_DB_NAME")

	if dbOpt == nil {
		dbOpt = new(DBOption)
	}
	if user != "" {
		dbOpt.User = user
	}
	if password != "" {
		dbOpt.Password = password
	}
	if host != "" {
		dbOpt.Host = host
	}
	if portStr != "" {
		port, _ := strconv.ParseUint(portStr, 10, 32)
		if port > 0 {
			dbOpt.Port = int(port)
		}
	}
	if dbOpt.Port == 0 {
		dbOpt.Port = 3306
	}
	if ccnetDbName != "" {
		dbOpt.CcnetDbName = ccnetDbName
	} else if dbOpt.CcnetDbName == "" {
		dbOpt.CcnetDbName = "ccnet_db"
		log.Infof("Failed to read SEAFILE_MYSQL_DB_CCNET_DB_NAME, use ccnet_db by default")
	}
	if seafileDbName != "" {
		dbOpt.SeafileDbName = seafileDbName
	} else if dbOpt.SeafileDbName == "" {
		dbOpt.SeafileDbName = "seafile_db"
		log.Infof("Failed to read SEAFILE_MYSQL_DB_SEAFILE_DB_NAME, use seafile_db by default")
	}
	return dbOpt
}
