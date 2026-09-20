package option

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SiloDrive/silo/fileserver/utils"
	log "github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
)

// InfiniteQuota is the ceiling that is not one: no limit at all.
//
// It is negative because zero is a real quota — an account allowed nothing —
// and it is -2 rather than -1 because it is stored rather than merely computed.
// It is what libmgr.AccountQuota returns, what the admin API serialises, and
// what sits in the quota column of every install that has ever run. Changing
// the number would re-read all of those rows as something else, silently.
const InfiniteQuota int64 = -2

// DefaultDiskReserve is the free space a server keeps back when nobody has
// said otherwise. See DiskReserve for why it is not zero.
const DefaultDiskReserve = 1 * GB

// DefaultMaxBufferedObjectBytes is how much the object lanes may hold in
// buffered whole objects at once, before they start refusing.
//
// An object is sealed and opened in one piece -- AES-GCM does not stream -- so
// every lane that touches one holds all of it. This is the total across every
// request in flight, which is the number that was missing: one request's
// ceiling says nothing about what eight of them cost.
//
// 512 MB, against a per-object ceiling of 128 MB, so four of the largest
// objects the server accepts fit at once and thousands of ordinary ones do.
// Raise it on a machine with room; the failure it prevents is the process
// being killed, which no request recovers from.
const DefaultMaxBufferedObjectBytes = 512 * MB

// Sizes here are decimal: a gigabyte is a thousand million bytes, not 1<<30.
//
// That is the unit a disk is sold in and the one an operator writing a quota is
// thinking in, so "100gb" in silo.conf means what it means on the invoice.
// Written as multiples of each other rather than as rows of zeroes, because the
// decimal choice is the whole point of the block and a literal 1000000000 does
// not show it.
const (
	KB = 1_000
	MB = 1000 * KB
	GB = 1000 * MB
	TB = 1000 * GB
)

var (
	// Host and Port are the address the server listens on. Both are set from
	// [fileserver] and then from the environment, which wins; see
	// resetToDefaults for why the compiled Host is loopback.
	Host string
	Port uint32

	// MaxUploadSize is the ceiling on one request body, in bytes, or zero when
	// nothing configured one. Zero is not "no limit" — it is the signal that
	// sends the caller to DefaultMaxUploadSize instead.
	MaxUploadSize uint64

	// MaxBufferedObjectBytes is how much memory the object lanes may hold in
	// buffered whole objects at once, across every request in flight.
	//
	// Zero means the compiled default. See DefaultMaxBufferedObjectBytes for
	// what the number is for.
	MaxBufferedObjectBytes int64

	// DefaultMaxUploadSize is the ceiling on one request's body when
	// max_upload_size says nothing, which is what an install that has never
	// read the config reference has.
	//
	// Nonzero, because the zero this used to be meant no limit: the shipped
	// behaviour of an unconfigured server was that one request could be any
	// size at all. It is a backstop and not the protection — a hundred
	// gigabytes is past any file this lane is meant to carry and short of
	// nothing an attacker wants, and what actually stops a body from taking
	// the volume is the reserve being watched while it arrives. Set
	// max_upload_size to mean it.
	DefaultMaxUploadSize uint64 = 100 * GB

	// EnableNotification serves /notification in-process. On by default;
	// SILO_ENABLE_NOTIFICATIONS=false closes it.
	EnableNotification bool

	// DefaultQuota is the ceiling an account gets when nothing gave it one of
	// its own, in bytes. InfiniteQuota is no ceiling, and is the default.
	DefaultQuota int64

	// ServerQuota bounds what every account together may hold, in the same
	// currency as DefaultQuota: logical size at head. InfiniteQuota is no
	// ceiling, and is the default.
	//
	// It is not DefaultQuota applied server-wide. That key is the default
	// *account* ceiling and has never been a statement about the server --
	// ten uncapped accounts, or capped accounts whose caps sum to more than
	// the disk, are exactly the case this exists for.
	ServerQuota int64

	// DiskReserve is how much free space the server refuses to spend, so that
	// the volume never reaches zero.
	//
	// Nonzero by default, which is a behaviour change for an install that has
	// never configured a quota, and deliberate: SQLite in WAL mode needs room
	// to write before it can commit, so a filesystem driven to the last block
	// takes the database down with it. Refusing a sync a gigabyte early is
	// recoverable; the other outcome is not. Set it to 0 to opt out.
	DiskReserve int64

	// DefaultKeepDays is how long a library keeps history when it has no
	// LibraryRetention row of its own. Zero means keep everything, which is
	// the default: a server that started expiring history because somebody
	// upgraded it would be deleting data nobody asked it to.
	DefaultKeepDays int

	// Build version (set by main)
	Version string

	// EnableProfiling publishes net/http/pprof, and ProfilePassword is what
	// gates it. Both are off and empty by default, and the server checks the
	// password is non-empty before it serves the routes at all — so the two
	// together are the switch, not EnableProfiling alone.
	EnableProfiling bool
	ProfilePassword string

	// LogLevel is a logrus level name, carried through as written. It is
	// validated where it is applied rather than here, so there is one place
	// that decides what an unreadable level falls back to.
	LogLevel string

	// DBOpTimeout bounds a single database call. See WithDBTimeout, which is
	// how it is meant to be reached.
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

	// AllowUserCreateLibrary says whether an account with the `user` role may
	// create a library on this install. It is the argument to
	// account.Role.MayCreateLibrary, and it is a statement about users only:
	// an admin is never subject to it, and a guest is never released by it.
	//
	// On by default, which is what every install has done until now and what
	// an ordinary deployment wants -- a user who cannot make a library has
	// nowhere to put anything that was not handed to them. Turning it off is
	// the curated shape docs/plans/sharing.md describes, where the admin owns
	// the libraries and everybody else syncs what they are given.
	//
	// Initialised here as well as in initDefaultOptions, because the bool zero
	// value is the restrictive answer: a caller that reaches the route without
	// loading options at all would otherwise refuse every user on an install
	// that never configured anything.
	AllowUserCreateLibrary = true
)

// resetToDefaults puts every option back to its compiled default, so that what
// a load produces depends on the config file and the environment and not on
// whatever a previous load left behind.
//
// Every option, which it did not used to be: MaxUploadSize, DefaultKeepDays,
// EnableProfiling, ProfilePassword, LogLevel and the hop count in utils were
// all left where they were. Today nothing calls the loader twice with a
// different config, so the leak is latent rather than live -- but it is the
// kind that stops being latent quietly. The moment anything reloads config on
// SIGHUP, a key deleted from the file goes on being obeyed, and the worst of
// them is EnableProfiling: a password-gated pprof endpoint that stays open
// after the lines that opened it are gone.
//
// Anything added to the var block above belongs here too. A default that
// exists only as an initialiser is one a second load cannot restore.
func resetToDefaults() {
	// Loopback by default. Silo speaks plaintext unless given a certificate,
	// and every credential it uses is a bearer token in a header, so a
	// default that publishes the port to the whole network is a default that
	// gives those tokens to anyone on it. Exposing the server is a decision
	// to make deliberately, with SILO_HOST or the config file — the Docker
	// image sets SILO_HOST=0.0.0.0 itself, since a container that binds
	// loopback cannot be reached through a published port at all.
	Host = "127.0.0.1"
	Port = 8082
	// Zero rather than DefaultMaxUploadSize: zero is how entries.go knows
	// nothing was configured, and it is the caller that supplies the fallback.
	MaxUploadSize = 0
	DefaultQuota = InfiniteQuota
	ServerQuota = InfiniteQuota
	DiskReserve = DefaultDiskReserve
	DefaultKeepDays = 0
	DBOpTimeout = time.Minute
	SyncObjectWrites = true
	VerifyFSObjectHashes = true
	LoginRateLimit = true
	TrustProxyHeaders = false
	EnableNotification = true
	AllowUserCreateLibrary = true
	EnableProfiling = false
	ProfilePassword = ""
	LogLevel = ""
	utils.TrustedProxyHops = utils.DefaultTrustedProxyHops
	initStorageDefaults()
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
		b, err := parseBool(v)
		if err != nil {
			log.Warnf("Ignoring unparseable %s=%q, using %v", name, v, def)
			return def
		}
		return b
	}
	return def
}

// envHops reads a proxy-hop count. Anything unparseable or below one keeps the
// default, because a bad value here is not a knob that fails to take effect --
// too high reads an entry the client wrote, which is the bug this exists to
// have fixed.
func envHops(def int, names ...string) int {
	for _, name := range names {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Warnf("Ignoring unparseable %s=%q, using %d", name, v, def)
			return def
		}
		return n
	}
	return def
}

// LoadFileServerOptions loads silo.conf from the given path. An empty
// path or a missing file is fine — Silo then runs entirely on compiled
// defaults plus environment variable overrides.
func LoadFileServerOptions(configFile string) {
	resetToDefaults()

	// SpaceBeforeInlineComment so that a value may contain a '#' or a ';' and
	// only a preceding space starts a comment. Passwords and paths do contain
	// them, and truncating one at the character would be silent.
	opts := ini.LoadOptions{SpaceBeforeInlineComment: true}

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
	// Which X-Forwarded-For entry is the client's, and so which one the rate
	// limiters count. It lives in utils because that is where the header is
	// read and there is no second copy to drift; see utils.TrustedProxyHops
	// for why the safe direction is downwards.
	utils.TrustedProxyHops = envHops(utils.TrustedProxyHops, "SILO_TRUSTED_PROXY_HOPS")

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

	// [httpserver] first and [fileserver] second, so that an install carrying
	// both has the current spelling win key by key rather than wholesale: a
	// [fileserver] that sets only the port leaves an [httpserver] host in
	// force, which is what a half-migrated config file means.
	for _, name := range []string{"httpserver", "fileserver"} {
		if section := sectionOf(config, name); section != nil {
			loadServerSection(section)
		}
	}

	// The environment beats the file for the listen address, and goes through
	// the same two parsers, so that SILO_PORT and `port =` agree on what a port
	// is. They did not: the variable was range-checked and the key was not.
	Host = fromEnv("SILO_HOST", Host, parseHost)
	Port = fromEnv("SILO_PORT", Port, parsePort)

	// Who may create a library. The section is the install's policy about
	// libraries rather than about the process, so it is not [fileserver].
	if section := sectionOf(config, "libraries"); section != nil {
		AllowUserCreateLibrary = fromSection(section, "allow_user_create_library",
			AllowUserCreateLibrary, parseBool)
	}
	AllowUserCreateLibrary = envBool(AllowUserCreateLibrary, "SILO_ALLOW_USER_CREATE_LIBRARY")

	if section := sectionOf(config, "history"); section != nil {
		DefaultKeepDays = fromSection(section, "keep_days", DefaultKeepDays, parseDays)
	}

	// Storage is loaded whether or not the section exists, because the
	// environment overrides apply either way: a deployment configured entirely
	// through SILO_* variables has no file for a section to be in.
	storageSection, _ := config.GetSection("storage")
	loadStorageOptions(storageSection)

	// All three quota keys take the same route as everything else, which is
	// what makes their answer to a bad value the same answer: keep the default
	// and say so. They did not used to agree -- two of them set InfiniteQuota
	// and warned separately, and the reserve had its own sign test, because
	// "keep -2 bytes free" would have disabled the protection over a typo.
	// Going through quotaSize gives the reserve that guard for nothing, since a
	// value it cannot read is an error rather than a negative number.
	if section := sectionOf(config, "quota"); section != nil {
		DefaultQuota = fromSection(section, "default", DefaultQuota, quotaSize)
		ServerQuota = fromSection(section, "server", ServerQuota, quotaSize)
		DiskReserve = fromSection(section, "reserve", DiskReserve, quotaSize)
	}

	LogLevel = fromEnv("SILO_LOG_LEVEL", LogLevel, parseText)
}

// sectionOf returns a section, or nil when the file has no such section.
//
// ini reports an absent section as an error, which reads as though something
// went wrong; nothing did, and a config file without a [quota] section is the
// ordinary case. Turning it into a nil says that, and says it the same way
// everywhere rather than four `err == nil` blocks whose meaning has to be
// reconstructed each time.
func sectionOf(config *ini.File, name string) *ini.Section {
	section, err := config.GetSection(name)
	if err != nil {
		return nil
	}
	return section
}

// loadServerSection reads the keys that describe the process itself: where it
// listens, how much of a request body it will take, and whether the profiler is
// open. Its caller applies it to [httpserver] and then [fileserver].
//
// Every value goes through fromSection and a parser, which is the same route
// the [storage] keys take. The point is not brevity: it is that a key which is
// present and unreadable keeps its default and says so out loud. Read one at a
// time, each key had its own answer to a bad value -- some kept the default
// silently, one narrowed it to a different number -- and none of them told
// anybody.
func loadServerSection(section *ini.Section) {
	Host = fromSection(section, "host", Host, parseHost)
	Port = fromSection(section, "port", Port, parsePort)
	MaxUploadSize = fromSection(section, "max_upload_size", MaxUploadSize, parseMegabytes)
	EnableProfiling = fromSection(section, "enable_profiling", EnableProfiling, parseBool)
	LogLevel = fromSection(section, "go_log_level", LogLevel, parseText)

	// Read only when profiling is on, so that a password left in the file does
	// not open the endpoint by itself.
	//
	// Refusing to start is the right end of the trade here, and the only place
	// in this file that takes it. An operator who asked for the profiler and
	// gave it no password gets a server that will not run; the alternative is a
	// server that runs with pprof reachable, which hands out heap contents and
	// goroutine stacks to anyone who finds the path. A missing password is not
	// a value to fall back from -- there is no safe default for it.
	//
	// The test is the key's absence, not its emptiness, which is deliberately
	// unchanged: `profile_password =` still starts, and the server then declines
	// to mount the routes at all because the password is empty. Two answers to
	// one mistake is not ideal, but tightening it here means a fatal that no
	// test can watch fail -- it takes the test binary down with the server --
	// so it stays as it was until it can be moved somewhere testable.
	if !EnableProfiling {
		return
	}
	key, err := section.GetKey("profile_password")
	if err != nil {
		log.Fatalf("[%s] enable_profiling is on with no profile_password: "+
			"pprof would be served to anyone who asks.", section.Name())
	}
	ProfilePassword = key.String()
}

// parseHost reads a listen address. Anything non-empty is taken as written --
// a name, a v4 address, a bracketed v6 one -- because what can be bound is the
// resolver's question and not this file's, and a host that does not resolve is
// reported by the listener with the error that says so.
//
// Empty is refused, and that is the whole reason this is a parser rather than a
// passthrough. "" makes the listen address ":8082", which is every interface,
// so `host =` -- a line emptied out, or a template that interpolated to nothing
// -- silently published a plaintext bearer-token API to the network. The least
// deliberate thing an operator can type should not produce the least
// conservative binding.
func parseHost(s string) (string, error) {
	host := strings.TrimSpace(s)
	if host == "" {
		return "", fmt.Errorf("is empty, which would listen on every interface; write an address like 127.0.0.1 or 0.0.0.0")
	}
	return host, nil
}

// parsePort reads a TCP port into the uint32 the field actually is.
//
// The width is the point. Read as a platform uint and then narrowed, a value
// past 2^32 did not fail -- it wrapped, so `port = 4294967297` became port 1 on
// a 64-bit build, a privileged port arrived at by arithmetic from a line that
// said nothing of the sort. Asking for 32 bits up front turns that into a
// refusal.
//
// Zero is allowed, because it is what a caller asking the kernel for any free
// port writes, and refusing it here would be a new rule rather than a fix.
func parsePort(s string) (uint32, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil || n > 65535 {
		return 0, fmt.Errorf("%q is not a port; write a whole number from 0 to 65535", s)
	}
	return uint32(n), nil
}

// parseMegabytes reads max_upload_size, which is written as a whole number of
// megabytes and stored in bytes.
//
// Decimal megabytes, the same MB the quota units use, so that two sizes in one
// config file do not mean two things. The overflow check answers the case the
// multiply used to wrap on: a ceiling that came out smaller than the number
// asked for is worse than one that was refused.
func parseMegabytes(s string) (uint64, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil || n > ^uint64(0)/MB {
		return 0, fmt.Errorf("%q is not a whole number of megabytes, like 100", s)
	}
	return n * MB, nil
}

// parseBool reads the spellings of yes and no that a config file or an
// environment variable might reasonably use. One list, shared with envBool, so
// that a value accepted in silo.conf is accepted in SILO_* as well.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "t", "1", "yes", "y", "on":
		return true, nil
	case "false", "f", "0", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a yes or a no; write true or false", s)
}

// parseText carries a value through with only its surrounding space removed,
// for the keys whose meaning is settled somewhere else. It never fails, which
// is the honest shape for them: rejecting a value here would be a second
// opinion about something this file does not decide.
func parseText(s string) (string, error) { return strings.TrimSpace(s), nil }

// parseDays reads a history retention in whole days. Negative is refused rather
// than read as a sentinel: zero already means "keep everything", so a minus
// sign is a typo and not a second way to say it.
func parseDays(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a whole number of days; write 0 to keep everything", s)
	}
	return n, nil
}

// quotaSize is parseQuota in the shape fromSection and fromEnv need.
//
// parseQuota can only answer InfiniteQuota for a value it could not read, and
// that answer is indistinguishable from the one it gives for a value that
// genuinely means "no ceiling" -- so on its own it cannot tell a caller whether
// a limit was configured and lost. This turns the unreadable case into an
// error, which is what lets the helper keep the default and name the key in the
// warning. silo#49 tracks parseQuota and ParseBytes becoming one parser, which
// is the change that would remove this.
func quotaSize(s string) (int64, error) {
	if n := parseQuota(s); n != InfiniteQuota {
		return n, nil
	}
	return 0, fmt.Errorf("%q is not a size; write a whole number with kb, mb, gb or tb, like 100gb", s)
}

// parseQuota turns a configured size into bytes, and answers InfiniteQuota for
// anything it cannot read.
//
// Normalised before it is read, because the failure of a strict parser here is
// silent and points the wrong way: an operator writing the unit the way it is
// usually written -- "900GB" -- got InfiniteQuota, which is to say no ceiling
// at all. A quota that comes to be unenforced without anybody noticing is the
// exact failure checkQuota's own comment says that code exists to prevent, and
// it was reachable from the config file by pressing shift.
//
// A bare number is gigabytes, which is what the multiplier defaults to and is
// kept for the installs that rely on it.
//
// InfiniteQuota for an unreadable value is still the only answer this can give
// -- it cannot invent the number the operator meant -- so quotaSize turns that
// answer into an error and the loader warns with the key still in hand. A
// warning rather than a refusal: a server that will not start because of a typo
// in a quota is worse than one that starts and says the quota is not in force,
// and the second is what an operator can act on at three in the morning.
func parseQuota(configured string) int64 {
	s := strings.ToLower(strings.TrimSpace(configured))

	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"kb", KB}, {"mb", MB}, {"gb", GB}, {"tb", TB}} {
		// end > 0 rather than >= 0: a string that is only a unit has no number
		// in front of it and is not a size.
		end := strings.Index(s, unit.suffix)
		if end <= 0 {
			continue
		}
		return scale(strings.TrimSpace(s[:end]), unit.size)
	}

	return scale(s, GB)
}

// scale multiplies a parsed number by its unit, and answers InfiniteQuota
// rather than a wrong number when it cannot.
//
// The overflow check is the point. A quota too large to hold in an int64 is
// not a large quota, it is a negative one, and everything downstream reads
// anything at or below zero as no ceiling at all -- so the arithmetic silently
// produces the opposite of what was configured. That is the same failure as
// reading "900GB" as unlimited, reached by typing a big number instead of a
// capital letter, and it is worth catching in the same place.
//
// user_quota_cmd.go's parseQuotaSize makes the same check for the same reason.
// Two parsers for one job is a real duplication -- see silo#49 -- and this one
// answers a sentinel where that one answers an error, which is why they are
// not yet one function.
func scale(digits string, unit int64) int64 {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n < 0 {
		return InfiniteQuota
	}
	if n > (1<<63-1)/unit {
		return InfiniteQuota
	}
	return n * unit
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
