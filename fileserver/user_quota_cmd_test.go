package silod

import (
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
)

// The cap this command writes has to be the one the write path reads. Not a
// row that looks right in the table: libmgr.AccountQuota is the function
// checkQuotaLocked calls before admitting a write, so asking it is the only
// check that says the setting took effect rather than merely landed.
func TestUserQuotaSetsACapTheWritePathReads(t *testing.T) {
	userTestStore(t)

	if err := setUserQuota(victim, "100gb"); err != nil {
		t.Fatalf("setUserQuota: %v", err)
	}

	quota, err := libmgr.AccountQuota(acctFor(t, victim).ID)
	if err != nil {
		t.Fatalf("AccountQuota: %v", err)
	}
	if want := int64(100 * option.GB); quota != want {
		t.Errorf("quota = %d, want %d", quota, want)
	}
}

// Setting a cap twice must land on the second one. The table's primary key is
// the account, so a plain INSERT fails on the second call and an operator
// raising somebody's quota would be told it worked while the old number stayed
// in force.
func TestUserQuotaCanBeRaisedAfterItIsSet(t *testing.T) {
	userTestStore(t)

	if err := setUserQuota(victim, "10gb"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := setUserQuota(victim, "50gb"); err != nil {
		t.Fatalf("second set: %v", err)
	}

	quota, err := libmgr.AccountQuota(acctFor(t, victim).ID)
	if err != nil {
		t.Fatalf("AccountQuota: %v", err)
	}
	if want := int64(50 * option.GB); quota != want {
		t.Errorf("quota = %d, want %d", quota, want)
	}
}

// A size this command cannot read is a refusal, never a silent unlimited.
//
// option.parseQuota answers InfiniteQuota for anything it fails to parse,
// which is the right answer for a config file nobody is watching and the wrong
// one for a command an operator is typing: "silo user quota alice 100gigs"
// would report success and remove the ceiling it was called to impose. The
// unit is required for the same reason -- a bare 100 means 100 GB in the
// config file, and an operator who reads it as 100 bytes has set a cap four
// orders of magnitude off with nothing to tell them.
func TestUserQuotaRefusesASizeItCannotRead(t *testing.T) {
	userTestStore(t)

	for _, size := range []string{"100gigs", "lots", "", "-5gb", "100"} {
		if err := setUserQuota(victim, size); err == nil {
			t.Errorf("setUserQuota(%q) was accepted, want an error", size)
		}
	}

	quota, err := libmgr.AccountQuota(acctFor(t, victim).ID)
	if err != nil {
		t.Fatalf("AccountQuota: %v", err)
	}
	if quota != option.InfiniteQuota {
		t.Errorf("quota = %d after refusals, want it still unset", quota)
	}
}

// Removing a cap is a distinct operation from setting a small one, and it has
// to leave AccountQuota answering unlimited rather than zero.
func TestUserQuotaNoneRemovesTheCap(t *testing.T) {
	userTestStore(t)

	if err := setUserQuota(victim, "10gb"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := setUserQuota(victim, "none"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	quota, err := libmgr.AccountQuota(acctFor(t, victim).ID)
	if err != nil {
		t.Fatalf("AccountQuota: %v", err)
	}
	if quota != option.InfiniteQuota {
		t.Errorf("quota = %d after removal, want unlimited", quota)
	}
}

// Reporting is a read. An operator asking what somebody's cap is must not
// change it, and must be told both numbers -- a quota without the usage
// beside it does not answer the question anybody asks it.
func TestUserQuotaReportsWithoutChangingAnything(t *testing.T) {
	userTestStore(t)

	if err := setUserQuota(victim, "10gb"); err != nil {
		t.Fatalf("set: %v", err)
	}

	var out string
	err := error(nil)
	out = captureStdout(t, func() { err = reportUserQuota(victim) })
	if err != nil {
		t.Fatalf("reportUserQuota: %v", err)
	}
	if !strings.Contains(out, "10 GB") {
		t.Errorf("report = %q, want the quota in it", out)
	}
	if !strings.Contains(out, "0 B") {
		t.Errorf("report = %q, want the usage in it", out)
	}

	quota, err := libmgr.AccountQuota(acctFor(t, victim).ID)
	if err != nil {
		t.Fatalf("AccountQuota: %v", err)
	}
	if want := int64(10 * option.GB); quota != want {
		t.Errorf("quota = %d after a report, want %d unchanged", quota, want)
	}
}

// An account that does not exist is an error, not a row created on the way
// past. The address is a foreign key in nine places and a quota against an
// account nobody can log into as is a cap that will never be consulted.
func TestUserQuotaRefusesAnUnknownAccount(t *testing.T) {
	userTestStore(t)

	if err := setUserQuota("nobody@example.com", "10gb"); err == nil {
		t.Error("setUserQuota against an unknown address was accepted, want an error")
	}
	if err := reportUserQuota("nobody@example.com"); err == nil {
		t.Error("reportUserQuota against an unknown address was accepted, want an error")
	}
}
