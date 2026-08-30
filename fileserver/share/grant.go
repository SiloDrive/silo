package share

// The grant model: one table that answers "may this principal do op at
// (library, path)".
//
// Before this there were three answers to that question -- SharedLibrary for a
// user, LibraryGroup for a group, InnerPubLibrary for everybody signed in --
// each read by its own function, in an order that was itself the policy. The
// invariant docs/plans/sharing.md asks for is that CheckPerm consults one
// model, and the reason is that the second reader is where the drift starts:
// a rule added to one lookup and not the others is a permission that holds on
// some paths and not others, and nothing says which.
//
// What has not changed is the answer. The precedence below is the old
// behaviour written down rather than inferred from return-statement order, and
// the tests that pinned it still pin it.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// Principal is who a grant is for: the kind and its identifier in one string.
//
// One string rather than a kind column beside a nullable id per kind, because
// the question this model is asked is "does any of these principals hold a
// grant here" -- which is one IN clause over a list, where three nullable
// columns would be three joins that can disagree.
type Principal string

// Anon is the principal every unauthenticated request carries. Nothing mints a
// grant to it yet; public libraries are the step that does, and the principal
// exists now so that step is a row rather than a fourth lookup.
const Anon Principal = "anon"

// UserPrincipal names one account.
func UserPrincipal(id account.ID) Principal { return Principal("user:" + id.String()) }

// GroupPrincipal names one group.
func GroupPrincipal(id int) Principal { return Principal(fmt.Sprintf("group:%d", id)) }

// LinkPrincipal names one share link's credential. Share links mint a real
// grant row so that the credential's perm stays a ceiling over a grant rather
// than becoming a grant itself, which is auth.md's rule and the reason links
// are not a special case inside CheckPerm.
func LinkPrincipal(credentialID string) Principal { return Principal("link:" + credentialID) }

// Kind reports the principal's kind without its identifier.
func (p Principal) Kind() string {
	if i := strings.IndexByte(string(p), ':'); i >= 0 {
		return string(p[:i])
	}
	return string(p)
}

// Grant is one row: a principal, a subtree, and what they may do in it.
type Grant struct {
	Principal Principal
	LibraryID string
	// Path is the subtree the grant reaches. "/" is the whole library.
	Path string
	// Perm is "r" or "rw". The credential's own perm column remains a ceiling
	// over this and never a grant.
	Perm string
	// Listed means something only for Anon: whether the library appears in the
	// public listing, as against being reachable only by a link.
	Listed    bool
	CreatedBy account.ID
	Ctime     int64
}

// rootPath is what a grant over a whole library carries.
const rootPath = "/"

// Add records a grant, replacing any the same principal already held on the
// same subtree.
//
// Replacing rather than adding, because two grants to one principal on one path
// are not two facts. Sharing again with a different permission is a correction,
// and a model that accumulated both rows would answer from whichever the query
// happened to reach first.
func Add(ctx context.Context, g Grant) error {
	if g.Perm != "r" && g.Perm != "rw" {
		return fmt.Errorf("a grant's permission is %q, want r or rw", g.Perm)
	}
	if g.Principal == "" || g.LibraryID == "" {
		return fmt.Errorf("a grant needs a principal and a library")
	}
	if g.Path == "" {
		g.Path = rootPath
	}
	if g.Ctime == 0 {
		g.Ctime = time.Now().Unix()
	}
	_, err := writeDB.ExecContext(ctx,
		`INSERT INTO LibraryGrant (principal, library_id, path, perm, listed, created_by, ctime)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(principal, library_id, path)
		 DO UPDATE SET perm = excluded.perm, listed = excluded.listed`,
		g.Principal, g.LibraryID, g.Path, g.Perm, g.Listed, nullID(g.CreatedBy), g.Ctime)
	if err != nil {
		return fmt.Errorf("recording a grant: %v", err)
	}
	return nil
}

// Remove deletes one grant. Removing one that is not there is not an error:
// the caller asked for a state, and it is the state they asked for.
func Remove(ctx context.Context, p Principal, libraryID, path string) error {
	if path == "" {
		path = rootPath
	}
	if _, err := writeDB.ExecContext(ctx,
		"DELETE FROM LibraryGrant WHERE principal = ? AND library_id = ? AND path = ?",
		p, libraryID, path); err != nil {
		return fmt.Errorf("removing a grant: %v", err)
	}
	return nil
}

// RemoveLibrary drops every grant on a library, for the moment the library
// itself goes. A grant naming a library nobody can reach is not harmful, but it
// is a row that outlives its subject and would reappear if the id were ever
// reused.
func RemoveLibrary(ctx context.Context, libraryID string) error {
	if _, err := writeDB.ExecContext(ctx,
		"DELETE FROM LibraryGrant WHERE library_id = ?", libraryID); err != nil {
		return fmt.Errorf("removing a library's grants: %v", err)
	}
	return nil
}

// ForLibrary lists what a library has been shared to, for the share surface.
func ForLibrary(ctx context.Context, libraryID string) ([]Grant, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT principal, library_id, path, perm, listed, created_by, ctime
		 FROM LibraryGrant WHERE library_id = ? ORDER BY path, principal`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("listing a library's grants: %v", err)
	}
	return scanGrants(rows)
}

// LibrariesFor lists the libraries these principals hold a whole-library grant
// on -- the shared-with-me question.
//
// Whole-library only. A subtree grant reaches a folder inside somebody else's
// library, which is not a library in this listing's sense; the virtual-library
// mechanism is what turns one of those into an entity with its own id, and it
// is that entity which appears.
func LibrariesFor(ctx context.Context, principals []Principal) (map[string]string, error) {
	if len(principals) == 0 {
		return map[string]string{}, nil
	}
	q := `SELECT library_id, perm FROM LibraryGrant
	      WHERE path = ? AND principal IN (` + placeholders(len(principals)) + `)`
	args := make([]any, 0, len(principals)+1)
	args = append(args, rootPath)
	for _, p := range principals {
		args = append(args, p)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing granted libraries: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var libraryID, perm string
		if err := rows.Scan(&libraryID, &perm); err != nil {
			return nil, err
		}
		// A library reached through two principals takes the stronger of them,
		// which is the same rule two groups already followed.
		if stronger(out[libraryID], perm) == perm {
			out[libraryID] = perm
		}
	}
	return out, rows.Err()
}

// permFor answers the model's question: given every principal a caller carries,
// what may they do at this path.
//
// The precedence is the old behaviour written down. It is deliberately not
// "the strongest grant anywhere wins", because it never was: an individual
// share answered and returned before a group share was looked at, so a user
// shared "r" directly reads "r" even while a group they belong to holds "rw".
// Surprising enough that a test pins it, and specific-beats-general is the rule
// that makes it defensible rather than accidental -- a grant naming you is a
// decision about you, and a grant naming a group you happen to be in is not.
//
// Within one kind the stronger permission wins, which is what two groups
// disagreeing already did.
func permFor(ctx context.Context, libraryID, path string, principals []Principal) (string, error) {
	if len(principals) == 0 {
		return "", nil
	}
	q := `SELECT principal, perm FROM LibraryGrant
	      WHERE library_id = ? AND path = ? AND principal IN (` + placeholders(len(principals)) + `)`
	args := make([]any, 0, len(principals)+2)
	args = append(args, libraryID, path)
	for _, p := range principals {
		args = append(args, p)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return "", fmt.Errorf("reading grants: %v", err)
	}
	defer func() { _ = rows.Close() }()

	byKind := map[string]string{}
	for rows.Next() {
		var p Principal
		var perm string
		if err := rows.Scan(&p, &perm); err != nil {
			return "", err
		}
		byKind[p.Kind()] = stronger(byKind[p.Kind()], perm)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	// Most specific first. A link is a decision about one request, a user
	// grant is a decision about one person, a group grant is a decision about
	// a set they belong to, and anon is a decision about everybody.
	for _, kind := range []string{"link", "user", "group", "anon"} {
		if perm := byKind[kind]; perm != "" {
			return perm, nil
		}
	}
	return "", nil
}

// stronger picks the wider of two permissions, treating an absent one as
// nothing. One function so that "rw beats r" is written once.
func stronger(a, b string) string {
	if a == "rw" || b == "rw" {
		return "rw"
	}
	if a == "r" || b == "r" {
		return "r"
	}
	return ""
}

// PrincipalsFor is every principal an account carries: itself, and each group
// it belongs to.
//
// Exported because the listing endpoints ask the same question the permission
// check does -- "what is this account, for the purposes of a grant" -- and two
// expansions of that would be the second reader all over again, this time
// between the answer a listing gives and the answer a fetch gives.
//
// Anon is deliberately not in this list. An account that holds no grant on a
// library must not be let in by a grant to everybody -- that is a separate
// decision the caller makes, because "signed in" and "anybody at all" are
// different audiences and the old InnerPubLibrary lookup treated them as one
// only outside cloud mode.
func PrincipalsFor(user account.ID) []Principal {
	out := []Principal{UserPrincipal(user)}
	groups, err := getGroupsByUser(user, false)
	if err != nil {
		log.Errorf("Failed to get groups for %s: %v", user, err)
		return out
	}
	for _, g := range groups {
		out = append(out, GroupPrincipal(g.id))
	}
	return out
}

func scanGrants(rows *sql.Rows) ([]Grant, error) {
	defer func() { _ = rows.Close() }()
	var out []Grant
	for rows.Next() {
		var g Grant
		var createdBy sql.Null[account.ID]
		if err := rows.Scan(&g.Principal, &g.LibraryID, &g.Path, &g.Perm,
			&g.Listed, &createdBy, &g.Ctime); err != nil {
			return nil, err
		}
		g.CreatedBy = createdBy.V
		out = append(out, g)
	}
	return out, rows.Err()
}

// nullID writes a zero account id as NULL rather than as sixteen zero bytes,
// so a grant with no recorded author does not look like one authored by an
// account that cannot exist.
func nullID(id account.ID) any {
	if id.IsZero() {
		return nil
	}
	return id
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// writeDB is the handle grants are written through. share.go has only ever
// held a read handle, because checking a permission is a read; recording one is
// not, and this is the first thing in the package that writes.
var writeDB *sql.DB

// ctxWithTimeout is the bounded context the package's own writes use when the
// caller has none of its own.
func ctxWithTimeout() (context.Context, context.CancelFunc) {
	return option.WithDBTimeout(context.Background())
}
