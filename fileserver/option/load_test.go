package option

import (
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/utils"
)

// These pin what LoadFileServerOptions does, key by key, before option.go is
// rewritten out of its inherited shape.
//
// The usual rule here is that a test comes first and is watched to fail. There
// is no bug to fail on: nothing about this behaviour is wrong, and the reason
// the file is being rewritten has nothing to do with what it does. So the rule
// is inverted rather than skipped -- these are written against the current
// parser and watched to PASS, and what would make them fail is the rewrite
// changing an answer. A test written after the rewrite would be a description
// of whatever came out of it, which proves nothing about what went in.
//
// [quota] and [storage] have their own files. This covers what was left:
// [fileserver], the [httpserver] alias, [libraries], [history], and the
// precedence between the file and the environment.

// The shipped server has never needed a config file. Every default is in the
// code, and the defaults that matter are the cautious ones: loopback rather
// than every interface, fsync on, hashes verified, logins throttled, proxy
// headers disbelieved.
func TestNoConfigFileLeavesEveryCompiledDefault(t *testing.T) {
	for _, path := range []string{"", "/nonexistent/silo.conf"} {
		LoadFileServerOptions(path)

		if Host != "127.0.0.1" {
			t.Errorf("Host = %q with no config at %q, want loopback", Host, path)
		}
		if Port != 8082 {
			t.Errorf("Port = %d with no config at %q, want 8082", Port, path)
		}
		if DefaultQuota != InfiniteQuota {
			t.Errorf("DefaultQuota = %d with no config, want InfiniteQuota", DefaultQuota)
		}
		if DBOpTimeout != 60*time.Second {
			t.Errorf("DBOpTimeout = %v with no config, want 60s", DBOpTimeout)
		}
		if !SyncObjectWrites {
			t.Error("SyncObjectWrites is off by default, and durability is meant to be opt-out")
		}
		if !VerifyFSObjectHashes {
			t.Error("VerifyFSObjectHashes is off by default, and the damage it prevents is unrepairable")
		}
		if !LoginRateLimit {
			t.Error("LoginRateLimit is off by default, which leaves password guessing unthrottled")
		}
		if TrustProxyHeaders {
			t.Error("TrustProxyHeaders is on by default, which hands an attacker a fresh identity per request")
		}
		if !EnableNotification {
			t.Error("EnableNotification is off by default, and /notification is meant to serve")
		}
		if !AllowUserCreateLibrary {
			t.Error("AllowUserCreateLibrary is off by default, which leaves users nowhere to put anything")
		}
	}
}

func TestTheFileserverSectionIsRead(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nhost = 0.0.0.0\nport = 9001\ngo_log_level = debug\n"))

	if Host != "0.0.0.0" {
		t.Errorf("[fileserver] host = %q, want 0.0.0.0", Host)
	}
	if Port != 9001 {
		t.Errorf("[fileserver] port = %d, want 9001", Port)
	}
	if LogLevel != "debug" {
		t.Errorf("[fileserver] go_log_level = %q, want debug", LogLevel)
	}
}

// max_upload_size is written in megabytes and stored in bytes, and the
// multiplier is the decimal million rather than 1<<20 -- the same MB the quota
// units use, so that two sizes in one config file do not mean two things.
func TestMaxUploadSizeIsReadAsMegabytes(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nmax_upload_size = 5\n"))

	if MaxUploadSize != 5*MB {
		t.Errorf("max_upload_size = 5 gave %d bytes, want %d", MaxUploadSize, 5*MB)
	}
}

// An unset ceiling is not an unlimited one. The zero this used to be meant a
// request could be any size at all, so the fallback is a real number that
// entries.go reaches for when nothing was configured.
func TestAnUnsetUploadCeilingFallsBackToARealNumber(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nport = 8082\n"))

	if MaxUploadSize != 0 {
		t.Errorf("MaxUploadSize = %d with nothing configured, want 0 so the caller takes the default", MaxUploadSize)
	}
	if DefaultMaxUploadSize == 0 {
		t.Error("DefaultMaxUploadSize is 0, which is the no-limit behaviour it exists to replace")
	}
}

// [httpserver] is the older spelling of the same section. Both are read and
// [fileserver] is read second, so a file carrying both means the newer one.
func TestTheOlderHttpserverSectionIsStillReadAndLosesToFileserver(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[httpserver]\nhost = 10.0.0.1\nport = 7001\n"))
	if Host != "10.0.0.1" || Port != 7001 {
		t.Errorf("[httpserver] gave host %q port %d, want 10.0.0.1 and 7001", Host, Port)
	}

	LoadFileServerOptions(writeConfig(t, "[httpserver]\nport = 7001\n\n[fileserver]\nport = 9001\n"))
	if Port != 9001 {
		t.Errorf("port = %d with both sections, want [fileserver] to win with 9001", Port)
	}
}

// The environment beats the file, so one binary can be pointed at a different
// deployment without editing anything.
func TestTheEnvironmentOverridesTheFile(t *testing.T) {
	conf := writeConfig(t, "[fileserver]\nhost = 127.0.0.1\nport = 9001\n")

	t.Setenv("SILO_HOST", "0.0.0.0")
	t.Setenv("SILO_PORT", "8100")
	LoadFileServerOptions(conf)

	if Host != "0.0.0.0" {
		t.Errorf("SILO_HOST did not override the file: Host = %q", Host)
	}
	if Port != 8100 {
		t.Errorf("SILO_PORT did not override the file: Port = %d", Port)
	}
}

// A port that is not a number leaves the configured one alone rather than
// falling to zero, which would be a server that binds whatever it is given.
func TestAnUnreadablePortEnvironmentKeepsTheConfiguredPort(t *testing.T) {
	t.Setenv("SILO_PORT", "not-a-port")
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nport = 9001\n"))

	if Port != 9001 {
		t.Errorf("Port = %d after an unreadable SILO_PORT, want the configured 9001", Port)
	}
}

// Who may make a library is the install's policy, so it is [libraries] rather
// than [fileserver]. Off is the curated shape: the admin owns the libraries and
// everyone else syncs what they are given.
func TestAllowUserCreateLibraryIsReadFromTheFileAndTheEnvironment(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[libraries]\nallow_user_create_library = false\n"))
	if AllowUserCreateLibrary {
		t.Error("[libraries] allow_user_create_library = false did not take")
	}

	t.Setenv("SILO_ALLOW_USER_CREATE_LIBRARY", "true")
	LoadFileServerOptions(writeConfig(t, "[libraries]\nallow_user_create_library = false\n"))
	if !AllowUserCreateLibrary {
		t.Error("SILO_ALLOW_USER_CREATE_LIBRARY did not override the file")
	}
}

// A value that is not a boolean keeps the default rather than reading as false,
// because false here is the restrictive answer and a typo should not quietly
// lock every user out of making a library.
func TestAnUnreadableLibraryPolicyKeepsTheDefault(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[libraries]\nallow_user_create_library = sometimes\n"))

	if !AllowUserCreateLibrary {
		t.Error("an unreadable allow_user_create_library turned the policy off, want the permissive default kept")
	}
}

// Zero keeps everything, and is the default: a server that started expiring
// history because somebody upgraded it would be deleting data nobody asked it
// to.
func TestKeepDaysIsReadAndDefaultsToKeepingEverything(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[history]\nkeep_days = 30\n"))
	if DefaultKeepDays != 30 {
		t.Errorf("[history] keep_days = %d, want 30", DefaultKeepDays)
	}

	LoadFileServerOptions(writeConfig(t, "[fileserver]\nport = 8082\n"))
	if DefaultKeepDays != 0 {
		t.Errorf("unconfigured keep_days = %d, want 0 so everything is kept", DefaultKeepDays)
	}
}

// Negative and unreadable both keep everything, for the same reason: neither is
// a number of days, and the safe reading of "I could not understand this" is to
// delete nothing.
func TestAnUnreadableKeepDaysDeletesNothing(t *testing.T) {
	for _, body := range []string{"[history]\nkeep_days = -1\n", "[history]\nkeep_days = a month\n"} {
		LoadFileServerOptions(writeConfig(t, body))
		if DefaultKeepDays != 0 {
			t.Errorf("keep_days = %d for %q, want 0 so nothing is expired", DefaultKeepDays, body)
		}
	}
}

// One parser for every boolean knob, because the hand-written `v == "false"`
// tests it replaced accepted two spellings and silently ignored the rest --
// SILO_TRUST_PROXY_HEADERS=yes did nothing at all, with nothing in the log.
func TestEveryWayOfWritingABooleanIsRead(t *testing.T) {
	for _, on := range []string{"true", "TRUE", "1", "yes", "YES", "on", "On"} {
		t.Setenv("SILO_TEST_BOOL", on)
		if !envBool(false, "SILO_TEST_BOOL") {
			t.Errorf("envBool did not read %q as true", on)
		}
	}
	for _, off := range []string{"false", "FALSE", "0", "no", "NO", "off", "Off"} {
		t.Setenv("SILO_TEST_BOOL", off)
		if envBool(true, "SILO_TEST_BOOL") {
			t.Errorf("envBool did not read %q as false", off)
		}
	}
	for _, junk := range []string{"maybe", "2", "-"} {
		t.Setenv("SILO_TEST_BOOL", junk)
		if !envBool(true, "SILO_TEST_BOOL") {
			t.Errorf("envBool(%q) did not keep the default", junk)
		}
	}
	t.Setenv("SILO_TEST_BOOL", "")
	if !envBool(true, "SILO_TEST_BOOL") {
		t.Error("an empty value did not keep the default")
	}
}

// The first name that is set wins, which is how the older spelling of a
// variable keeps working without overriding the newer one.
func TestTheFirstBooleanNameThatIsSetWins(t *testing.T) {
	t.Setenv("SILO_TEST_NEW", "false")
	t.Setenv("SILO_TEST_OLD", "true")
	if envBool(true, "SILO_TEST_NEW", "SILO_TEST_OLD") {
		t.Error("the older name overrode the newer one")
	}
}

// Too low is safe and too high is not: too low groups more clients into one
// rate-limit bucket than it should, too high reads an entry the client wrote
// and hands them a key they choose. So anything that is not a count above zero
// keeps the default rather than being clamped into one.
func TestAnUnreadableProxyHopCountKeepsTheDefault(t *testing.T) {
	for _, junk := range []string{"0", "-1", "lots", ""} {
		t.Setenv("SILO_TEST_HOPS", junk)
		if got := envHops(3, "SILO_TEST_HOPS"); got != 3 {
			t.Errorf("envHops(%q) = %d, want the default 3 kept", junk, got)
		}
	}
	t.Setenv("SILO_TEST_HOPS", "2")
	if got := envHops(1, "SILO_TEST_HOPS"); got != 2 {
		t.Errorf("envHops(\"2\") = %d, want 2", got)
	}
}

// The hop count lives in utils, beside the header it decides how to read, and
// the loader reaches across to set it. That makes it one more thing a load has
// to put back: a count left over from a previous load is a proxy topology the
// config file no longer describes, and reading the wrong X-Forwarded-For entry
// is how an attacker picks their own rate-limit key.
func TestTheProxyHopCountGoesBackToTheDefaultWhenNothingSetsIt(t *testing.T) {
	t.Setenv("SILO_TRUSTED_PROXY_HOPS", "3")
	LoadFileServerOptions("")
	if utils.TrustedProxyHops != 3 {
		t.Fatalf("TrustedProxyHops = %d, want the 3 the environment asked for", utils.TrustedProxyHops)
	}

	t.Setenv("SILO_TRUSTED_PROXY_HOPS", "")
	LoadFileServerOptions("")
	if utils.TrustedProxyHops != utils.DefaultTrustedProxyHops {
		t.Errorf("TrustedProxyHops = %d after a load that set nothing, want the %d default back",
			utils.TrustedProxyHops, utils.DefaultTrustedProxyHops)
	}
}

// Profiling publishes a password-gated endpoint, so the password is read only
// when profiling is on.
//
// Not tested: profiling on with no password calls log.Fatal, which takes the
// process down. That is deliberate -- an open pprof endpoint is worse than a
// server that refuses to start -- and it is why this case is described here
// rather than exercised, since a test that asserts it would exit the test
// binary. The rewrite has to keep it.
func TestTheProfilingPasswordIsReadWhenProfilingIsOn(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nenable_profiling = true\nprofile_password = hunter2\n"))

	if !EnableProfiling {
		t.Error("enable_profiling = true did not take")
	}
	if ProfilePassword != "hunter2" {
		t.Errorf("profile_password = %q, want hunter2", ProfilePassword)
	}
}

// Profiling off leaves the password unread, so a password sitting in the file
// does not open the endpoint on its own.
func TestProfilingIsOffUnlessItIsAskedFor(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nprofile_password = hunter2\n"))

	if EnableProfiling {
		t.Error("profiling turned itself on because a password was present")
	}
}

// MaxBufferedObjectBytes is documented as a knob -- "zero means the compiled
// default" -- and nothing in the tree ever assigns it. There is no config key
// and no environment variable, so objectbudget.go has always taken the default
// and the variable is unreachable configuration.
//
// Pinned as it stands rather than fixed here, so that the rewrite neither
// quietly wires it up nor quietly drops it. Wiring it is a separate change with
// a separate argument about what the key should be called.
func TestTheObjectBufferCeilingIsNotActuallyConfigurable(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nmax_buffered_object_bytes = 64\n"))

	if MaxBufferedObjectBytes != 0 {
		t.Errorf("MaxBufferedObjectBytes = %d; if this is now settable, the comment and the docs need to say how", MaxBufferedObjectBytes)
	}
}

// An empty host is not "listen everywhere", but that is what it did.
//
// `host =` with nothing after it is a line somebody wrote and then emptied, or
// a template that interpolated to nothing. It set Host to "", which makes the
// listen address ":8082", which is every interface on the machine -- so the
// least deliberate thing an operator can type produced the least conservative
// binding, and did it silently. A key that is present and empty says nothing
// about where to listen, so the compiled default stands.
func TestAnEmptyHostDoesNotPublishTheServerToEveryInterface(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nhost =\n"))

	if Host != "127.0.0.1" {
		t.Errorf("host = %q for an empty [fileserver] host, want the loopback default kept", Host)
	}
}

// A port that does not fit the field is refused, not truncated.
//
// Port is a uint32 and the value was read as a platform uint, so on a 64-bit
// build `port = 4294967297` parsed happily and then narrowed to 1 -- a
// privileged port, arrived at by arithmetic, from a line that said nothing of
// the sort. Anything that cannot be a port keeps the default.
func TestAPortTooLargeForTheFieldIsRefusedRatherThanTruncated(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nport = 4294967297\n"))

	if Port != 8082 {
		t.Errorf("port = 4294967297 gave Port = %d, want the 8082 default kept", Port)
	}
}
