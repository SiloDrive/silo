package silod

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
)

// RunUser is the account lifecycle, on the host, without a running server.
//
// Until now an account could be created two ways -- SILO_ADMIN_EMAIL with a
// password beside it, or the bootstrap admin the server mints when the table
// is empty -- and changed no way at all. Everything after the first account
// meant opening silo.db and writing SQL, which is a poor thing to ask of an
// operator and a worse thing for them to get wrong: the password column wants
// a hash in a particular format, and the address is a foreign key in nine
// places.
//
// It is a CLI rather than an API for the same two reasons "silo token" is.
// There is no admin role in the API layer yet to gate such an endpoint with,
// and the moment this is most needed is the one where nobody can log in --
// the IdP is down, or the only password is lost. Anyone who can run this
// already owns the data directory, so it grants nothing they did not have.
//
// docs/roadmap.md's admin API is the same operations over HTTP, and
// is meant to be built on these functions rather than beside them.
func RunUser(args []string) error {
	flags := commandFlags("user")
	// Without this, "silo user -h" prints the flags and never names a
	// subcommand — which is the one thing somebody typing it wants.
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, UserUsage)
		fmt.Fprintln(os.Stderr, "\nflags:")
		flags.PrintDefaults()
	}
	staff := flags.Bool("staff", false, "with add: give the account the is_staff flag")
	generate := flags.Bool("generate", false, "with add or passwd: invent a password and print it once")
	jsonOut := flags.Bool("json", false, "with list: output as JSON")
	rest, done, err := parseCommandArgs("user", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) == 0 {
		return errors.New(UserUsage)
	}
	action := rest[0]

	// The invocation is resolved to the thing it will run before the database
	// is opened. A typo should answer with the usage text, not create a data
	// directory somewhere the operator did not mean to point at.
	//
	// One switch rather than two: an earlier version validated the arity in
	// one switch and dispatched in another, which meant every subcommand was
	// named twice and a subcommand added to the first but forgotten in the
	// second fell through the dispatch default and enabled an account.
	var run func() error
	switch action {
	case "list":
		run = func() error { return listUsers(*jsonOut) }
	case "quota":
		// One name, two operations, because "what is it" and "set it to" are
		// the same question asked with and without an answer. Splitting them
		// into quota and set-quota would mean an operator who typed the
		// reading form with a size got the usage text instead of the change.
		if len(rest) < 2 {
			return fmt.Errorf("silo user quota needs an email address\n\n%s", UserUsage)
		}
		if len(rest) > 3 {
			return fmt.Errorf("silo user quota takes an email address and at most one size\n\n%s", UserUsage)
		}
		email := rest[1]
		if len(rest) == 2 {
			run = func() error { return reportUserQuota(email) }
			break
		}
		size := rest[2]
		run = func() error { return setUserQuota(email, size) }
	case "add", "passwd", "disable", "enable":
		// Every one of these names one person, and names them by the address
		// the operator knows rather than the id the tables hold.
		if len(rest) < 2 {
			return fmt.Errorf("silo user %s needs an email address\n\n%s", action, UserUsage)
		}
		email := rest[1]
		switch action {
		case "add":
			run = func() error { return addUser(email, *staff, *generate) }
		case "passwd":
			run = func() error { return passwdUser(email, *generate) }
		case "disable":
			run = func() error { return setUserActive(email, false) }
		case "enable":
			run = func() error { return setUserActive(email, true) }
		}
	default:
		return fmt.Errorf("unknown user subcommand %q\n\n%s", action, UserUsage)
	}

	if err := openStores(); err != nil {
		return err
	}
	account.Init(siloPair.Read, siloPair.Write)
	// passwd revokes what the account holds, so this command needs the
	// credential store wired even though most of its subcommands do not. A
	// package left uninitialised does not fail to compile and does not fail
	// gracefully: the nil *sql.DB panics on first use.
	credential.Init(siloPair.Read, siloPair.Write)

	return run()
}

// UserUsage spells the flags before the subcommand because that is where
// parseCommandArgs insists they go, for the reason its own comment gives: a
// flag written after the positionals is not a parse error, it is ignored, and
// a command that changes a password against the wrong data directory without
// saying so is worse than one that refuses.
const UserUsage = `usage:
  silo user [-json] list                      Show every account
  silo user [-staff] [-generate] add <email>  Create an account
  silo user [-generate] passwd <email>        Set a password
  silo user disable <email>                   Stop every credential it holds
  silo user enable <email>                    Undo a disable
  silo user quota <email>                     Show the cap and what is used
  silo user quota <email> <size|none>         Set the cap: 100gb, 500mb, none

Flags come first: silo user -generate add alice@example.com`

func listUsers(asJSON bool) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	users, err := account.List(ctx)
	if err != nil {
		return err
	}

	if asJSON {
		return printUsersJSON(users)
	}
	if len(users) == 0 {
		fmt.Println("No accounts. The server will mint a bootstrap admin on its next start.")
		return nil
	}

	// tabwriter measures every column, not just the widest address. A listing
	// an operator has to read with a ruler is one they will read wrong, and
	// hand-computed widths only ever measure the column somebody remembered.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tSTATUS\tSTAFF\tPASSWORD\tCREATED")
	for _, u := range users {
		status := "active"
		if !u.IsActive {
			status = "DISABLED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			displayEmail(u), status, yesNo(u.IsStaff), yesNo(u.HasPassword), formatTime(u.Ctime))
	}
	return tw.Flush()
}

// displayEmail names an account that has no primary address. Create always
// writes one, so this should never print -- which is the reason it says
// something rather than leaving a blank column that reads as a formatting bug.
func displayEmail(u account.Listed) string {
	if u.Email == "" {
		return "(no address: " + u.ID.String() + ")"
	}
	return u.Email
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func addUser(email string, staff, generate bool) error {
	norm := account.Normalize(email)
	if norm == "" {
		return errors.New("no email address given")
	}

	// Ask before prompting. Finding out that the address is taken after
	// typing a password twice is a waste of the operator's time, and hashing
	// one we are about to throw away costs 600k PBKDF2 iterations.
	// authmgr.CreateAccount asks again and still decides — this is courtesy,
	// not the guard against a race.
	ctx, cancel := option.WithDBTimeout(context.Background())
	if existing, err := account.ByEmail(ctx, norm); err == nil {
		cancel()
		return fmt.Errorf("%s already exists; use \"silo user passwd %s\" to change the password",
			existing.Email, existing.Email)
	}
	cancel()

	password, generated, err := readNewPassword(generate, norm)
	if err != nil {
		return err
	}

	// A fresh context: the read above was bounded before the operator started
	// typing, and a prompt has no deadline.
	writeCtx, writeCancel := option.WithDBTimeout(context.Background())
	defer writeCancel()
	created, err := authmgr.CreateAccount(writeCtx, norm, password, staff)
	if err != nil {
		return err
	}
	if !created {
		// Somebody else claimed the address between the check above and here.
		// Their password is the real one; ours was never stored.
		return fmt.Errorf("%s was created by something else while this ran; nothing was changed", norm)
	}

	if staff {
		fmt.Printf("Created staff account %s.\n", norm)
	} else {
		fmt.Printf("Created %s.\n", norm)
	}
	announceGenerated(password, generated)
	return nil
}

func passwdUser(email string, generate bool) error {
	acct, err := resolveAccount(email)
	if err != nil {
		return err
	}

	password, generated, err := readNewPassword(generate, acct.Email)
	if err != nil {
		return err
	}

	setCtx, setCancel := option.WithDBTimeout(context.Background())
	defer setCancel()
	if err := authmgr.SetAccountPassword(setCtx, acct.ID, password); err != nil {
		return err
	}

	// An administrator reset revokes everything, per docs/auth.md. The two
	// cases genuinely differ: a user changing their own password should revoke
	// sessions and leave devices mounted, because unmounting somebody's laptop
	// as a side effect of routine hygiene teaches them to stop doing hygiene.
	// This is not that case. Reaching this command means shell access to the
	// server and an account that is not yours to log in to, and the reason an
	// administrator resets a password is that the user has lost control of
	// something -- which something is not knowable from here.
	//
	// It happens after the password is set rather than before: a revocation
	// that ran and then failed to change the password would sign every device
	// out and leave the old password working, which is the worst of both.
	revoked, err := credential.RevokeAll(setCtx, acct.ID)
	if err != nil {
		return fmt.Errorf("password set for %s, but revoking their credentials failed: %v",
			acct.Email, err)
	}

	fmt.Printf("Password set for %s.\n", acct.Email)
	announceGenerated(password, generated)

	fmt.Printf("\nRevoked %d credential%s: every device and session signed out, from the next\n"+
		"request. They sign in again with the new password.\n", revoked, pluralS(revoked))
	return nil
}

func setUserActive(email string, active bool) error {
	acct, err := resolveAccount(email)
	if err != nil {
		return err
	}
	if acct.IsActive == active {
		fmt.Printf("%s is already %s.\n", acct.Email, activeWord(active))
		return nil
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := account.SetActive(ctx, acct.ID, active); err != nil {
		return err
	}

	if active {
		fmt.Printf("Enabled %s. The tokens they held still work; disabling never revoked them.\n", acct.Email)
		return nil
	}

	// Disabling is the one operation that stops every lane at once, which is
	// worth saying out loud: it is why an operator reaches for this instead
	// of revoking tokens one store at a time.
	fmt.Printf("Disabled %s. Every credential they hold stops working — sessions, device\n"+
		"credentials and all — from the next request, and starts again if the account\nis re-enabled.\n", acct.Email)
	return nil
}

func activeWord(active bool) string {
	if active {
		return "enabled"
	}
	return "disabled"
}

func announceGenerated(password string, generated bool) {
	if !generated {
		return
	}
	fmt.Printf("\nGenerated password: %s\n", password)
	fmt.Println("This is the only time it is shown. It is not stored anywhere in this form.")
}

// readNewPassword gets a password without one ever appearing on a command
// line.
//
// There is deliberately no -password flag. auth.md's finding 9 is about
// SILO_ADMIN_PASSWORD sitting in docker-compose.yml and in the environment of
// a running container; a flag would be the same mistake in a shorter-lived
// place, readable by every other process on the host through /proc and kept
// in the operator's shell history afterwards.
//
// So: -generate invents one, a terminal is prompted twice with echo off, and
// anything else reads one line from stdin, which is what a script or a
// password manager pipes in.
func readNewPassword(generate bool, who string) (password string, generated bool, err error) {
	if generate {
		password, err = authmgr.GeneratePassword()
		return password, true, err
	}

	if term.IsTerminal(int(stdin.Fd())) {
		password, err = promptPassword("New password for " + who + ": ")
		return password, false, err
	}

	password, err = readPasswordLine(stdin)
	return password, false, err
}

// stdin is where a password comes from when it is not being generated. It is
// a variable so that a test can hand these commands a pipe and exercise the
// same path an operator's shell does; nothing in the program reassigns it.
var stdin = os.Stdin

func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	first, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %v", err)
	}

	fmt.Print("Again: ")
	second, err := term.ReadPassword(int(stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %v", err)
	}

	return confirmPassword(string(first), string(second))
}

// confirmPassword is the part of the prompt that has a decision in it, split
// out from the terminal handling so it can be tested without one.
func confirmPassword(first, second string) (string, error) {
	if first != second {
		return "", errors.New("the two passwords do not match; nothing was changed")
	}
	if first == "" {
		return "", errors.New("an empty password would leave an account no password can open; use -generate instead")
	}
	return first, nil
}

// readPasswordLine takes one line, and only strips the line ending. Leading
// and trailing spaces are part of a password if somebody chose them, and a
// password manager that emits one should not have it silently changed here
// into something that will not log in.
func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("reading password from stdin: %v", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on stdin; pipe one in or pass -generate")
	}
	return line, nil
}

// userJSON is the listing's wire shape, named here rather than inlined so
// that a field cannot be renamed by accident.
type userJSON struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	IsActive    bool   `json:"is_active"`
	IsStaff     bool   `json:"is_staff"`
	HasPassword bool   `json:"has_password"`
	Created     string `json:"created"`
}

func printUsersJSON(users []account.Listed) error {
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, userJSON{
			ID:          u.ID.String(),
			Email:       u.Email,
			IsActive:    u.IsActive,
			IsStaff:     u.IsStaff,
			HasPassword: u.HasPassword,
			Created:     formatTime(u.Ctime),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
