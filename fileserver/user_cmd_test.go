package silod

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/option"
)

// userTestStore is the token harness plus authmgr, because most of what these
// commands are for is only observable by trying to log in afterwards.
func userTestStore(t *testing.T) {
	t.Helper()
	tokenTestStore(t)
	authmgr.Init(siloPair.Read, siloPair.Write)
}

// withStdin hands a command a password the way a pipe would. A pipe is not a
// terminal, so this exercises the same branch as `echo … | silo user passwd`.
func withStdin(t *testing.T, text string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		_, _ = io.WriteString(w, text)
		_ = w.Close()
	}()

	orig := stdin
	stdin = r
	t.Cleanup(func() {
		stdin = orig
		_ = r.Close()
	})
}

// captureStdout collects what a command printed, so a test can check that the
// generated password it announced is the one that actually works.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()

	fn()

	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func TestAddUserCreatesAnAccountThatCanLogIn(t *testing.T) {
	userTestStore(t)
	withStdin(t, "correct horse battery staple\n")

	if err := addUser("newcomer@example.com", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	acct, err := authmgr.ValidatePassword("newcomer@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatalf("the account this created cannot log in: %v", err)
	}
	if acct.Role != account.RoleUser {
		t.Errorf("an account created without -role is %q, want user", acct.Role)
	}
	if !acct.IsActive {
		t.Error("a newly created account is not active")
	}
}

func TestAddUserStoresAHashRatherThanThePassword(t *testing.T) {
	userTestStore(t)
	withStdin(t, "hunter2\n")

	if err := addUser("hashed@example.com", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, stored, err := account.PasswordHash(ctx, "hashed@example.com")
	if err != nil {
		t.Fatalf("reading the stored password: %v", err)
	}
	if strings.Contains(stored, "hunter2") {
		t.Fatalf("the plaintext password is in the database: %q", stored)
	}
	// The format is self-describing on purpose: AccountPassword has no algo
	// column, so validatePasswd dispatches on this prefix.
	if !strings.HasPrefix(stored, "PBKDF2SHA256$") {
		t.Errorf("stored password is %q, want a PBKDF2SHA256 hash", stored)
	}
}

// The address is normalised on the way in, or `silo user add Alice@…` and a
// login as `alice@…` become two different accounts — the exact failure the
// identity split exists to prevent.
func TestAddUserNormalizesTheAddress(t *testing.T) {
	userTestStore(t)
	withStdin(t, "hunter2\n")

	if err := addUser("  Alice@Example.COM  ", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	acct, err := account.ByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("the account is not reachable by its normalised address: %v", err)
	}
	if acct.Email != "alice@example.com" {
		t.Errorf("stored address is %q, want it lowercased and trimmed", acct.Email)
	}
}

// Refusing is the point: `add` on an existing address must not quietly become
// a password reset, because an operator who typed the wrong address would
// then have changed a password they did not mean to touch.
func TestAddUserRefusesAnExistingAddressWithoutChangingIt(t *testing.T) {
	userTestStore(t)

	withStdin(t, "first-password\n")
	if err := addUser("twice@example.com", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	withStdin(t, "second-password\n")
	err := addUser("TWICE@example.com", "user", false)
	if err == nil {
		t.Fatal("addUser accepted an address that already exists")
	}
	if !strings.Contains(err.Error(), "silo user passwd") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}

	if _, err := authmgr.ValidatePassword("twice@example.com", "first-password"); err != nil {
		t.Errorf("the original password stopped working: %v", err)
	}
	if _, err := authmgr.ValidatePassword("twice@example.com", "second-password"); err == nil {
		t.Error("the refused second password was stored anyway")
	}
}

func TestAddUserRoleFlag(t *testing.T) {
	userTestStore(t)
	withStdin(t, "hunter2\n")

	if err := addUser("boss@example.com", "admin", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	acct, err := account.ByEmail(ctx, "boss@example.com")
	if err != nil {
		t.Fatalf("no account: %v", err)
	}
	if !acct.Role.IsAdmin() {
		t.Errorf("-role admin created a %q account", acct.Role)
	}
}

// A generated password is shown once and never again, so the thing worth
// pinning is that what was printed is what was stored.
func TestGeneratedPasswordIsTheOneThatWorks(t *testing.T) {
	userTestStore(t)

	var addErr error
	out := captureStdout(t, func() {
		addErr = addUser("generated@example.com", "user", true)
	})
	if addErr != nil {
		t.Fatalf("addUser returned %v", addErr)
	}

	password := generatedFrom(t, out)
	if _, err := authmgr.ValidatePassword("generated@example.com", password); err != nil {
		t.Fatalf("the password that was printed does not log in: %v", err)
	}
}

func generatedFrom(t *testing.T, out string) string {
	t.Helper()
	const marker = "Generated password: "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no generated password in the output:\n%s", out)
	}
	line := out[i+len(marker):]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if line == "" {
		t.Fatal("the generated password line is empty")
	}
	return line
}

func TestPasswdReplacesThePassword(t *testing.T) {
	userTestStore(t)

	withStdin(t, "old-password\n")
	if err := addUser("rotate@example.com", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	withStdin(t, "new-password\n")
	if err := passwdUser("rotate@example.com", false); err != nil {
		t.Fatalf("passwdUser returned %v", err)
	}

	if _, err := authmgr.ValidatePassword("rotate@example.com", "new-password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if _, err := authmgr.ValidatePassword("rotate@example.com", "old-password"); err == nil {
		t.Error("the old password still works")
	}
}

func TestPasswdRefusesAnUnknownAddress(t *testing.T) {
	userTestStore(t)
	withStdin(t, "hunter2\n")

	if err := passwdUser("nobody@example.com", false); err == nil {
		t.Fatal("passwdUser accepted an address with no account")
	}
}

// Disabling is the operation the identity split was for: one flag, and every
// lane refuses. The account row is what the lanes read, so that is what this
// checks.
func TestDisableAndEnableFlipTheAccount(t *testing.T) {
	userTestStore(t)

	withStdin(t, "hunter2\n")
	if err := addUser("lapsed@example.com", "user", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}

	if err := setUserActive("lapsed@example.com", false); err != nil {
		t.Fatalf("disable returned %v", err)
	}
	if _, err := authmgr.ValidatePassword("lapsed@example.com", "hunter2"); err == nil {
		t.Error("a disabled account can still log in")
	}

	if err := setUserActive("lapsed@example.com", true); err != nil {
		t.Fatalf("enable returned %v", err)
	}
	if _, err := authmgr.ValidatePassword("lapsed@example.com", "hunter2"); err != nil {
		t.Errorf("re-enabling did not restore the login: %v", err)
	}
}

// Disabling stops credentials without deleting them, so re-enabling restores
// the user's devices rather than making everyone log in again. That is a
// deliberate difference from `silo token revoke`, and it is only true while
// nothing here quietly deletes rows.
//
// What makes disabling take effect is the account join inside Resolve, not a
// deletion here — see credential.Resolve and the ErrInactive path it returns.
func TestDisableLeavesTheCredentialsInPlace(t *testing.T) {
	userTestStore(t)

	const q = "SELECT COUNT(*) FROM Credential WHERE account_id = ?"
	before := countRows(t, q, acctFor(t, victim).ID)
	if before == 0 {
		t.Fatal("the fixture seeded no credentials, so this proves nothing")
	}

	if err := setUserActive(victim, false); err != nil {
		t.Fatalf("disable returned %v", err)
	}

	if n := countRows(t, q, acctFor(t, victim).ID); n != before {
		t.Errorf("disabling deleted credentials: %d then %d", before, n)
	}
}

func TestDisableRefusesAnUnknownAddress(t *testing.T) {
	userTestStore(t)

	if err := setUserActive("nobody@example.com", false); err == nil {
		t.Fatal("disable accepted an address with no account")
	}
}

func TestListReportsWhatAnOperatorNeedsToDecide(t *testing.T) {
	userTestStore(t)

	withStdin(t, "hunter2\n")
	if err := addUser("listed@example.com", "admin", false); err != nil {
		t.Fatalf("addUser returned %v", err)
	}
	if err := setUserActive("listed@example.com", false); err != nil {
		t.Fatalf("disable returned %v", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	users, err := account.List(ctx)
	if err != nil {
		t.Fatalf("List returned %v", err)
	}

	var found bool
	for _, u := range users {
		if u.Email != "listed@example.com" {
			continue
		}
		found = true
		if u.IsActive {
			t.Error("a disabled account lists as active")
		}
		if !u.Role.IsAdmin() {
			t.Errorf("an admin account lists as %q", u.Role)
		}
		if !u.HasPassword {
			t.Error("an account with a password lists as having none")
		}
		if u.Ctime == 0 {
			t.Error("the account has no creation time")
		}
	}
	if !found {
		t.Fatal("the account is missing from the listing")
	}
}

// An account with no AccountPassword row cannot be opened by any password,
// and an operator asked "why can they not log in" should be able to see that
// rather than infer it.
func TestListDistinguishesAnAccountWithNoPassword(t *testing.T) {
	userTestStore(t)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, "identity-only@example.com", "", account.RoleUser); err != nil {
		t.Fatalf("create: %v", err)
	}

	users, err := account.List(ctx)
	if err != nil {
		t.Fatalf("List returned %v", err)
	}
	for _, u := range users {
		if u.Email == "identity-only@example.com" && u.HasPassword {
			t.Error("an account with no password row lists as having one")
		}
	}
}

// A password manager that emits a value with surrounding spaces has chosen
// those spaces. Trimming them here would store a hash of something the user
// will never type again.
func TestReadPasswordLineKeepsSurroundingSpaces(t *testing.T) {
	got, err := readPasswordLine(strings.NewReader("  spaced out  \n"))
	if err != nil {
		t.Fatalf("readPasswordLine returned %v", err)
	}
	if got != "  spaced out  " {
		t.Errorf("readPasswordLine = %q, want the spaces kept", got)
	}
}

func TestReadPasswordLineStripsBothLineEndings(t *testing.T) {
	for _, in := range []string{"hunter2\n", "hunter2\r\n", "hunter2"} {
		got, err := readPasswordLine(strings.NewReader(in))
		if err != nil {
			t.Fatalf("readPasswordLine(%q) returned %v", in, err)
		}
		if got != "hunter2" {
			t.Errorf("readPasswordLine(%q) = %q", in, got)
		}
	}
}

// An empty password would leave an account whose stored hash no typed
// password can ever match, which is a lockout dressed as a success.
func TestReadPasswordLineRefusesAnEmptyPassword(t *testing.T) {
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := readPasswordLine(strings.NewReader(in)); err == nil {
			t.Errorf("readPasswordLine(%q) accepted an empty password", in)
		}
	}
}

// The confirmation is the whole point of asking twice: a mistyped password
// that is stored anyway locks the account out silently, and the operator
// finds out when the user cannot log in.
func TestConfirmPassword(t *testing.T) {
	if got, err := confirmPassword("hunter2", "hunter2"); err != nil || got != "hunter2" {
		t.Errorf("confirmPassword on a matching pair = %q, %v", got, err)
	}
	if _, err := confirmPassword("hunter2", "hunter3"); err == nil {
		t.Error("confirmPassword accepted two different passwords")
	}
	if _, err := confirmPassword("", ""); err == nil {
		t.Error("confirmPassword accepted an empty password")
	}
}

func TestRunUserRejectsUnknownSubcommands(t *testing.T) {
	userTestStore(t)

	for _, args := range [][]string{
		{"delete", "someone@example.com"},
		{"add"}, // no address
	} {
		if err := RunUser(args); err == nil {
			t.Errorf("RunUser(%q) accepted a bad invocation", args)
		}
	}
}

// An operator resetting a password is an administrator reset, and docs/auth.md
// is explicit that the two cases differ: a user changing their own password
// revokes sessions and leaves devices mounted, because unmounting somebody's
// laptop as a side effect of routine hygiene teaches them to stop doing
// hygiene. An administrator reset revokes everything, because the reason an
// administrator resets a password is that the user has lost control of
// something and which something is not knowable from here.
//
// This command is the second case: it is reached by someone with shell access
// to the server, acting on an account that is not theirs to log in to.
func TestPasswdRevokesEveryCredential(t *testing.T) {
	userTestStore(t)

	const q = "SELECT COUNT(*) FROM Credential WHERE account_id = ?"
	if before := countRows(t, q, acctFor(t, victim).ID); before == 0 {
		t.Fatal("the fixture seeded no credentials, so this proves nothing")
	}

	withStdin(t, "a whole new password\n")
	if err := passwdUser(victim, false); err != nil {
		t.Fatalf("passwdUser returned %v", err)
	}

	if n := countRows(t, q, acctFor(t, victim).ID); n != 0 {
		t.Errorf("%d credentials survived a password reset, want 0", n)
	}
	// Resetting one account's password must not sign the rest of the server out.
	if n := countRows(t, q, acctFor(t, bystander).ID); n != 1 {
		t.Errorf("bystander holds %d credentials, want 1", n)
	}
}

// A typo in -role is caught before the operator is asked to type a password
// twice. account.CreateTx checks the role as well and is the guard that
// matters, but it is reached on the far side of the prompt -- so this test
// gives the command no password at all: reaching the prompt is the failure.
func TestAnUnknownRoleIsRefusedBeforeThePasswordPrompt(t *testing.T) {
	userTestStore(t)
	withStdin(t, "")

	err := addUser("typo@example.com", "administrator", false)
	if err == nil {
		t.Fatal("addUser accepted the role \"administrator\"")
	}
	if !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("addUser failed with %v, want the unknown-role refusal", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := account.ByEmail(ctx, "typo@example.com"); err == nil {
		t.Error("a refused role still created an account")
	}
}
