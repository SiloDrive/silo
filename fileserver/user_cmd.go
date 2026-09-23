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

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/admin"
	"github.com/SiloDrive/silo/fileserver/authmgr"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/internal/format"
)

// RunUser is the account lifecycle, on the host, without a running server.
//
// The first account is made by claiming the setup token, through the TUI. This
// is everything after that one, and the reason it exists is that there used to
// be nothing: an account could be created two ways, both of them at first boot,
// and changed no way at all. Everything after the first account
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
	roleFlag := flags.String("role", string(account.DefaultRole), "with add: admin, user, or guest")
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
	case "grant", "revoke":
		// Two arguments, and the second is a set rather than a single name:
		// handing somebody users and quota in two commands leaves a window in
		// which they hold half of what the operator decided to give them.
		if len(rest) != 3 {
			return fmt.Errorf("silo user %s needs an email address and a comma-separated list of capabilities\n\n%s",
				action, UserUsage)
		}
		email, caps := rest[1], rest[2]
		if action == "grant" {
			run = func() error { return grantUser(email, caps) }
		} else {
			run = func() error { return revokeUser(email, caps) }
		}
	case "add", "passwd", "disable", "enable":
		// Every one of these names one person, and names them by the address
		// the operator knows rather than the id the tables hold.
		if len(rest) < 2 {
			return fmt.Errorf("silo user %s needs an email address\n\n%s", action, UserUsage)
		}
		email := rest[1]
		switch action {
		case "add":
			run = func() error { return addUser(email, *roleFlag, *generate) }
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
	admin.Init(siloPair.Read, siloPair.Write)

	return run()
}

// UserUsage spells the flags before the subcommand because that is where
// parseCommandArgs insists they go, for the reason its own comment gives: a
// flag written after the positionals is not a parse error, it is ignored, and
// a command that changes a password against the wrong data directory without
// saying so is worse than one that refuses.
const UserUsage = `usage:
  silo user [-json] list                      Show every account
  silo user [-role <role>] [-generate] add <email>  Create an account
                                              (role: admin, user, guest)
  silo user [-generate] passwd <email>        Set a password
  silo user disable <email>                   Stop every credential it holds
  silo user enable <email>                    Undo a disable
  silo user quota <email>                     Show the cap and what is used
  silo user quota <email> <size|none>         Set the cap: 100gb, 500mb, none
  silo user grant <email> <caps>              Give administrative capabilities
  silo user revoke <email> <caps>             Take them away
                                              (caps: users, passwords, quota,
                                               tokens, retention, grant)

Flags come first: silo user -generate add alice@example.com`

func listUsers(asJSON bool) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	users, err := account.List(ctx)
	if err != nil {
		return err
	}
	// Read whatever the listing is about to render, in one query rather than
	// one per account. Capabilities mean nothing on an account that is not an
	// admin -- admin.Can is a conjunction -- but the rows are shown as they
	// are, because a listing that hid them would hide exactly what a demotion
	// left behind.
	caps, err := admin.OfAll(ctx)
	if err != nil {
		return err
	}

	if asJSON {
		return printUsersJSON(users, caps)
	}
	if len(users) == 0 {
		fmt.Println("No accounts. The server will mint a bootstrap admin on its next start.")
		return nil
	}

	// tabwriter measures every column, not just the widest address. A listing
	// an operator has to read with a ruler is one they will read wrong, and
	// hand-computed widths only ever measure the column somebody remembered.
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "EMAIL\tSTATUS\tROLE\tPASSWORD\tCREATED\tCAPABILITIES")
	for _, u := range users {
		status := "active"
		if !u.IsActive {
			status = "DISABLED"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			displayEmail(u), status, string(u.Role), yesNo(u.HasPassword),
			format.Time(u.Ctime), admin.Join(caps[u.ID]))
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

func addUser(email, roleName string, generate bool) error {
	norm := account.Normalize(email)
	if norm == "" {
		return errors.New("no email address given")
	}

	// Before the prompt, not after it. account.CreateTx checks the role too and
	// is the guard that matters, but it is reached on the far side of the
	// operator typing a password twice -- and a typo in a flag is not worth
	// making them do that to find out about.
	role, err := account.ParseRole(roleName)
	if err != nil {
		return err
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
	created, err := authmgr.CreateAccount(writeCtx, norm, password, role)
	if err != nil {
		return err
	}
	if !created {
		// Somebody else claimed the address between the check above and here.
		// Their password is the real one; ours was never stored.
		return fmt.Errorf("%s was created by something else while this ran; nothing was changed", norm)
	}

	fmt.Printf("Created %s as %s.\n", norm, role)
	announceGenerated(password, generated)
	return nil
}

// warnAboutKeyMaterial says what a reset just did to an account's end-to-end
// encryption, and it is deliberately specific about which of two situations
// the operator is in.
//
// An operator cannot re-wrap the identity key on the user's behalf, and that
// is the design working rather than a gap: re-wrapping means first unwrapping,
// which needs the old password. Whoever is resetting a password does not have
// it — that is why they are resetting it. A server that could do this could
// also read the key, and every library it opens.
//
// So the identity blob is left in place and unopenable, and what happens next
// depends on something the operator cannot see from the prompt: whether the
// user ever published recovery wraps. Saying "use a recovery code" to somebody
// who has none is worse than saying nothing, because it sends them looking for
// a card that was never printed.
func warnAboutKeyMaterial(email string, keys *account.Keys, err error) {
	if err != nil || keys == nil {
		// ErrNoKeys is the ordinary case: an account with no end-to-end
		// encryption has nothing here to lose. Anything else has already been
		// reported by the write that mattered.
		return
	}
	fmt.Printf("\n%s has published an identity key, and it was wrapped under the old password.\n", email)
	fmt.Printf("The new password does not open it, and this server cannot re-wrap it — doing that\n")
	fmt.Printf("would mean reading the key, which is the one thing it is built not to do.\n")
	if len(keys.Recovery) > 0 {
		fmt.Printf("\nThey hold %d recovery wrap(s): redeeming one recovers the identity key, after\n", len(keys.Recovery))
		fmt.Printf("which their client re-wraps it under the new password and republishes.\n")
		return
	}
	fmt.Printf("\nThey have no recovery wraps, so there is no way back to that identity key.\n")
	fmt.Printf("They must enrol again, and every end-to-end encrypted library that key opened\n")
	fmt.Printf("stays sealed unless another member re-shares it.\n")
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

	// Read before the write, because the write is what makes it untrue.
	keys, keysErr := account.GetKeys(setCtx, acct.ID)

	if err := authmgr.SetAccountPassword(setCtx, acct.ID, password); err != nil {
		return err
	}
	warnAboutKeyMaterial(acct.Email, keys, keysErr)

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
	// admin.SetActive rather than account.SetActive: disabling the last
	// account able to administer this server is refused at the CLI too. The
	// invariant is about the install rather than about the caller, and the
	// operator can enable somebody else first -- which is the thing they meant
	// to do.
	if err := admin.SetActive(ctx, acct.ID, active); err != nil {
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
// There is deliberately no -password flag. auth.md's finding 9 was about a
// bootstrap password sitting in docker-compose.yml and in the environment of a
// running container -- the argument that has since removed SILO_ADMIN_PASSWORD
// altogether in favour of the setup token. A flag would be the same mistake in
// a shorter-lived place, readable by every other process on the host through
// /proc and kept in the operator's shell history afterwards.
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
	ID           string   `json:"id"`
	Email        string   `json:"email"`
	IsActive     bool     `json:"is_active"`
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
	HasPassword  bool     `json:"has_password"`
	Created      string   `json:"created"`
}

func printUsersJSON(users []account.Listed, caps map[account.ID][]admin.Capability) error {
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		// An empty list rather than null: a consumer iterating the field
		// should not have to special-case the account that holds nothing,
		// which is most of them.
		held := []string{}
		for _, c := range caps[u.ID] {
			held = append(held, string(c))
		}
		out = append(out, userJSON{
			ID:           u.ID.String(),
			Email:        u.Email,
			IsActive:     u.IsActive,
			Role:         string(u.Role),
			Capabilities: held,
			HasPassword:  u.HasPassword,
			Created:      format.Time(u.Ctime),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// grantUser and revokeUser are the install's own hand on administrative
// authority: no actor, because the operator already holds the database and an
// actor check would be a formality asked of somebody who could write the row
// directly. What they do honour is the last-holder-of-grant invariant, which
// is about the install rather than about the caller -- and which costs nothing
// here, since handing grant to somebody else first is the thing the operator
// meant to do.
//
// The whole set is parsed before the account is looked up, so a typo in a
// capability name is reported as a typo rather than as a partial grant.
func grantUser(email, list string) error {
	caps, acct, err := resolveCapabilityChange(email, list)
	if err != nil {
		return err
	}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := admin.Assign(ctx, acct.ID, caps...); err != nil {
		return err
	}
	return reportCapabilities(ctx, acct)
}

func revokeUser(email, list string) error {
	caps, acct, err := resolveCapabilityChange(email, list)
	if err != nil {
		return err
	}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := admin.Withdraw(ctx, acct.ID, caps...); err != nil {
		return err
	}
	return reportCapabilities(ctx, acct)
}

// resolveCapabilityChange parses the set and finds the account, in that order.
// Parsing first means an unknown capability costs a message and not a database
// read, and -- more to the point -- it means no part of a set containing one is
// ever written.
func resolveCapabilityChange(email, list string) ([]admin.Capability, *account.Account, error) {
	caps, err := admin.ParseCapabilities(list)
	if err != nil {
		return nil, nil, err
	}
	acct, err := resolveAccount(email)
	if err != nil {
		return nil, nil, err
	}
	return caps, acct, nil
}

// reportCapabilities prints what the account holds now rather than what
// changed. The operator asked for a state, and the set they are looking at is
// the answer to "did that do what I meant".
func reportCapabilities(ctx context.Context, acct *account.Account) error {
	held, err := admin.Of(ctx, acct.ID)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		fmt.Printf("%s holds no administrative capabilities.\n", acct.Email)
		return nil
	}
	fmt.Printf("%s holds: %s\n", acct.Email, admin.Join(held))
	if !acct.Role.IsAdmin() {
		// Said out loud, because the rows are real and do nothing: an operator
		// who granted a capability and saw it listed would otherwise have no
		// way to learn why the person still cannot use it.
		fmt.Printf("\n%s is a %s, not an admin, so none of these grant anything yet.\n",
			acct.Email, acct.Role)
		fmt.Printf("Capabilities are half of the rule: the account must be an admin as well.\n")
	}
	return nil
}
