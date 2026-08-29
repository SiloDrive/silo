package option

import (
	"context"
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

	// GroupTableName is the table groups are read from.
	//
	// Configurable because upstream let the group table be provisioned
	// externally under another name. Silo creates the schema itself, so the
	// only value that works with a Silo-created database is the default —
	// point it elsewhere and the table has to already exist.
	GroupTableName string

	// quota options
	DefaultQuota int64

	// DefaultKeepDays is how long a library keeps history when it has no
	// LibraryRetention row of its own. Zero means keep everything, which is
	// the default: a server that started expiring history because somebody
	// upgraded it would be deleting data nobody asked it to.
	DefaultKeepDays int

	// redis options
	HasRedisOptions bool
	RedisHost       string
	RedisPasswd     string
	RedisPort       uint32
	RedisExpiry     uint32
	RedisMaxConn    uint32
	RedisTimeout    time.Duration

	// Build version (set by main)
	Version string

	// Profile password
	ProfilePassword string
	EnableProfiling bool

	// Go log level
	LogLevel string

	// DB default timeout
	DBOpTimeout time.Duration

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

	// VerifyFSObjectHashes checks that an uploaded fs object hashes to the id
	// it was sent under, before it is stored.
	//
	// Only fs objects need an option. A block id is the SHA-1 of exactly the
	// bytes stored, so that check is free and unconditional; a commit is
	// checked by comparing two ids already in hand. An fs object's id is the
	// SHA-1 of its uncompressed JSON while the stored form is compressed, so
	// verifying costs an inflate per object — the one case where an operator
	// on slow hardware might reasonably decline.
	//
	// On by default, for the same reason object writes are fsynced by
	// default: what it prevents is silent, permanent and shared. An object
	// stored under the wrong id is never rewritten, because every later
	// writer sees the id already present, and it is served to everyone using
	// that store.
	VerifyFSObjectHashes = true

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

	JWTPrivateKey string

	// metric
	NodeName string
)

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
	SyncObjectWrites = true
	VerifyFSObjectHashes = true
	LoginRateLimit = true
	TrustProxyHeaders = false
	EnableNotification = true
}

// envBool reads the first of names that is set and parses it as a boolean,
// returning def when none is set or the value makes no sense.
//
// One parser for all of them, because the alternative failed quietly: a
// hand-written `v == "false"` test accepts "false" and "0" and silently
// ignores "no" and "off", and each knob picked its own polarity — so
// SILO_TRUST_PROXY_HEADERS=yes did nothing at all, with nothing in the log to
// say why, and the resulting symptom (every client sharing one rate-limit
// bucket) is exactly what setting it was meant to prevent.
func envBool(def bool, names ...string) bool {
	for _, name := range names {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		switch strings.ToLower(v) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
		log.Warnf("Ignoring unparseable %s=%q, using %v", name, v, def)
		return def
	}
	return def
}

// LoadFileServerOptions loads silo.conf from the given path. An empty
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
	EnableNotification = envBool(EnableNotification,
		"SILO_ENABLE_NOTIFICATIONS", "ENABLE_NOTIFICATION_SERVER")

	// Accepts a Go duration ("5m", "30s") or "0" to disable the auth caches
	// entirely. Unlike the token TTL, a wrong value here is recoverable by
	// fixing it and restarting, so it is clamped rather than rejected: a
	// negative duration means the same thing as zero.
	LoginRateLimit = envBool(LoginRateLimit, "SILO_LOGIN_RATE_LIMIT")
	if !LoginRateLimit {
		log.Warn("SILO_LOGIN_RATE_LIMIT is off: password guessing against the login " +
			"endpoints is unthrottled.")
	}

	TrustProxyHeaders = envBool(TrustProxyHeaders, "SILO_TRUST_PROXY_HEADERS")

	VerifyFSObjectHashes = envBool(VerifyFSObjectHashes, "SILO_VERIFY_FS_OBJECT_HASHES")
	if !VerifyFSObjectHashes {
		log.Warn("SILO_VERIFY_FS_OBJECT_HASHES is off: an fs object stored under " +
			"the wrong id will not be detected, and cannot be repaired afterwards.")
	}

	// Durability of object writes. Opt-out only — an operator has to say so
	// explicitly, because the failure it protects against is silent and
	// unrecoverable rather than merely inconvenient.
	SyncObjectWrites = envBool(SyncObjectWrites, "SILO_SYNC_OBJECT_WRITES")
	if !SyncObjectWrites {
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
	if envHost := os.Getenv("SILO_HOST"); envHost != "" {
		Host = envHost
	}
	if envPort := os.Getenv("SILO_PORT"); envPort != "" {
		if port, err := strconv.ParseUint(envPort, 10, 32); err == nil {
			Port = uint32(port)
		}
	}

	if section, err := config.GetSection("history"); err == nil {
		if key, err := section.GetKey("keep_days"); err == nil {
			if days, err := key.Int(); err == nil && days >= 0 {
				DefaultKeepDays = days
			} else {
				log.Warnf("[history] keep_days = %q is not a whole number of days; keeping all history", key.String())
			}
		}
	}

	if section, err := config.GetSection("quota"); err == nil {
		if key, err := section.GetKey("default"); err == nil {
			quotaStr := key.String()
			DefaultQuota = parseQuota(quotaStr)
		}
	}

	loadCacheOptionFromEnv()

	GroupTableName = os.Getenv("SILO_GROUP_TABLE_NAME")
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
	// One name, no fallback. This read used to also accept JWT_PRIVATE_KEY,
	// the unprefixed spelling from before the rename, and a fallback like that
	// never ends: nothing expires it and nothing tells an operator which of the
	// two names their server is actually signing with.
	JWTPrivateKey = os.Getenv("SILO_JWT_SECRET")
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

	return nil
}

// WithDBTimeout is the context a database call would otherwise build for
// itself. It lives here, beside the deadline it applies, so that reaching for
// it does not mean importing a package that owns a table.
//
// It takes the parent rather than assuming context.Background() so that the
// request-scoped callers can use it too: a query made on behalf of a request
// should die with the request as well as at the deadline. A helper only half
// the call sites could adopt would be a second convention, not one rule.
func WithDBTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, DBOpTimeout)
}
