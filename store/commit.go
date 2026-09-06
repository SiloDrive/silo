package store

import (
	"crypto/sha256"
	"fmt"
)

// CommitVersion is the version byte written into commit objects.
const CommitVersion = 1

// Commit is one point in a library's history.
type Commit struct {
	// Root is the root directory object's id, and it is PUBLIC in both
	// library types — never sealed. It has to be: the root is the first edge
	// of every server-side walk, GC's mark and changes?since= included, and a
	// sealed root would leave the server unable to trace an E2EE library at
	// all.
	//
	// The anti-splice property survives because the seal *binds* the root
	// rather than hiding it: root and parents sit inside the one
	// authenticated range, and the splice only ever worked while a sealed
	// blob was portable between commits.
	Root      ID
	Parents   []ID
	CreatedAt int64
	// Author and Message are public in a plain library and sealed under
	// E2EE — so plain-library history does not regress to anonymous commits
	// and E2EE libraries do not publish who did what and when.
	Author  string
	Message string
}

// Validate reports whether the commit can be encoded.
func (c *Commit) Validate() error {
	if len(c.Parents) > MaxParents {
		return fmt.Errorf("%w: %d parents, above %d", ErrEncoding, len(c.Parents), MaxParents)
	}
	if len(c.Author) > MaxAuthorBytes {
		return fmt.Errorf("%w: author is %d bytes, above %d", ErrEncoding, len(c.Author), MaxAuthorBytes)
	}
	if len(c.Message) > MaxMessageBytes {
		return fmt.Errorf("%w: message is %d bytes, above %d", ErrEncoding, len(c.Message), MaxMessageBytes)
	}
	return nil
}

// Encode encodes the commit for a plain library.
func (c *Commit) Encode() ([]byte, error) { return c.encode(nil) }

// EncodeSealed encodes the commit for an E2EE library, sealing the author and
// message under its content key.
func (c *Commit) EncodeSealed(ck []byte) ([]byte, error) {
	if err := checkCK(ck, "sealing a commit"); err != nil {
		return nil, err
	}
	return c.encode(ck)
}

func (c *Commit) encode(ck []byte) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	e2ee := ck != nil

	var flags byte
	if e2ee {
		flags |= flagE2EE
	}
	out := []byte{CommitVersion, flags}
	out = append(out, c.Root[:]...)
	out = appendUvarint(out, uint64(len(c.Parents)))
	for _, p := range c.Parents {
		out = append(out, p[:]...)
	}
	out = appendUvarint(out, uint64(clampTimestamp(c.CreatedAt)))

	// The two free-text fields, written the same way in both places — public
	// here, sealed below.
	attribution := func(b []byte) []byte {
		b = appendUvarint(b, uint64(len(c.Author)))
		b = append(b, c.Author...)
		b = appendUvarint(b, uint64(len(c.Message)))
		return append(b, c.Message...)
	}

	if !e2ee {
		out = attribution(out)
		if len(out) > MaxCommitBytes {
			return nil, fmt.Errorf("%w: encoded commit is %d bytes, above the %d ceiling",
				ErrEncoding, len(out), MaxCommitBytes)
		}
		return out, nil
	}

	// The sealed section is always present and always carries both fields,
	// even when both are empty — one shape rather than two, so no port has to
	// decide what an absent sealed section means.
	sealed := attribution(nil)
	sealHash := sha256.Sum256(sealed)
	out = append(out, sealHash[:]...)
	frame, err := sealSection(ck, domainCommit, out, sealed)
	if err != nil {
		return nil, err
	}
	out = append(out, frame...)
	if len(out) > MaxCommitBytes {
		return nil, fmt.Errorf("%w: encoded commit is %d bytes, above the %d ceiling",
			ErrEncoding, len(out), MaxCommitBytes)
	}
	return out, nil
}

// DecodeCommit reads a plain library's commit.
func DecodeCommit(b []byte) (*Commit, error) { return decodeCommit(b, nil) }

// DecodeSealedCommit reads an E2EE library's commit and opens its sealed
// section.
func DecodeSealedCommit(b, ck []byte) (*Commit, error) {
	if err := checkCK(ck, "opening a commit"); err != nil {
		return nil, err
	}
	return decodeCommit(b, ck)
}

// PublicCommit is a commit as a server can see it, with no content key.
//
// Root is here because it is public in both library types and has to be: it is
// the first edge of every server-side walk, and a sealed root would leave a
// server unable to trace an E2EE library at all. Parents and the timestamp
// come with it, which is what history traversal needs.
//
// Author and message are not here in either library type. Under E2EE they are
// sealed; in a plain library they are readable through DecodeCommit, and this
// reader deliberately reports the same fields whichever type it is handed, so
// that a server path written against it cannot come to depend on a library
// being unencrypted.
type PublicCommit struct {
	E2EE      bool
	Root      ID
	Parents   []ID
	CreatedAt int64
}

// DecodeCommitPublic reads the public section of a commit of either library
// type, without a content key.
//
// It completes the set: manifests give up their chunk list, directories their
// edges, and commits their root and parents, all without a key. That is the
// server's whole view of an E2EE library, and it is exactly enough to store,
// serve, trace and reclaim one — and not enough to read it.
func DecodeCommitPublic(b []byte) (*PublicCommit, error) {
	c, e2ee, _, err := parseCommitPublic(b)
	if err != nil {
		return nil, err
	}
	return &PublicCommit{E2EE: e2ee, Root: c.Root, Parents: c.Parents, CreatedAt: c.CreatedAt}, nil
}

func decodeCommit(b, ck []byte) (*Commit, error) {
	c, declared, p, err := parseCommitPublic(b)
	if err != nil {
		return nil, err
	}
	e2ee := ck != nil
	if declared != e2ee {
		return nil, fmt.Errorf("%w: commit declares E2EE=%t, library is E2EE=%t",
			ErrEncoding, declared, e2ee)
	}

	if !e2ee {
		if err := c.readAttribution(b, &p); err != nil {
			return nil, err
		}
		if p != len(b) {
			return nil, fmt.Errorf("%w: %d bytes past the end of a plain commit",
				ErrEncoding, len(b)-p)
		}
		return c, nil
	}

	if len(b)-p < IDSize {
		return nil, fmt.Errorf("%w: commit ends before its seal hash", ErrEncoding)
	}
	var sealHash ID
	copy(sealHash[:], b[p:p+IDSize])
	p += IDSize

	plain, err := openSection(ck, domainCommit, b[:p], b[p:])
	if err != nil {
		return nil, err
	}
	return c.openAttribution(plain, sealHash)
}

// parseCommitPublic reads the part of a commit that needs no key, and returns
// it, the library type the object declares, and how far it got. Both decoders
// start here; what follows is the caller's rule.
func parseCommitPublic(b []byte) (*Commit, bool, int, error) {
	if len(b) > MaxCommitBytes {
		return nil, false, 0, fmt.Errorf("%w: commit is %d bytes, above the %d ceiling",
			ErrEncoding, len(b), MaxCommitBytes)
	}
	if len(b) < 2+IDSize+1 {
		return nil, false, 0, fmt.Errorf("%w: commit is %d bytes, too short for a header", ErrEncoding, len(b))
	}
	if b[0] != CommitVersion {
		return nil, false, 0, fmt.Errorf("%w: commit version %d, this build writes %d",
			ErrEncoding, b[0], CommitVersion)
	}
	flags := b[1]
	if flags&^byte(flagE2EE) != 0 {
		return nil, false, 0, fmt.Errorf("%w: commit reserved flag bits are set (%#02x)", ErrEncoding, flags)
	}
	e2ee := flags&flagE2EE != 0

	c := &Commit{}
	p := 2
	copy(c.Root[:], b[p:p+IDSize])
	p += IDSize

	count, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, false, 0, fmt.Errorf("%w: commit parent count", ErrEncoding)
	}
	p += n
	if count > MaxParents {
		return nil, false, 0, fmt.Errorf("%w: commit claims %d parents, above %d", ErrEncoding, count, MaxParents)
	}
	if uint64(len(b)-p) < count*IDSize {
		return nil, false, 0, fmt.Errorf("%w: commit ends inside its parent list", ErrEncoding)
	}
	c.Parents = make([]ID, count)
	for i := range c.Parents {
		copy(c.Parents[i][:], b[p:])
		p += IDSize
	}

	created, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, false, 0, fmt.Errorf("%w: commit timestamp", ErrEncoding)
	}
	p += n
	if created > MaxTimestamp {
		return nil, false, 0, fmt.Errorf("%w: commit timestamp %d above the %d ceiling",
			ErrEncoding, created, uint64(MaxTimestamp))
	}
	c.CreatedAt = int64(created)
	return c, e2ee, p, nil
}

// openAttribution reads the author and message out of an opened sealed
// section, checking that it is the section this commit says it has.
func (c *Commit) openAttribution(plain []byte, sealHash ID) (*Commit, error) {
	if sha256.Sum256(plain) != sealHash {
		return nil, fmt.Errorf("%w: commit seal hash does not match its sealed section", ErrEncoding)
	}
	q := 0
	if err := c.readAttribution(plain, &q); err != nil {
		return nil, err
	}
	if q != len(plain) {
		return nil, fmt.Errorf("%w: %d bytes past the end of the sealed section",
			ErrEncoding, len(plain)-q)
	}
	return c, nil
}

func (c *Commit) readAttribution(b []byte, p *int) error {
	author, err := readBounded(b, p, MaxAuthorBytes, ErrEncoding, "commit", "author")
	if err != nil {
		return err
	}
	message, err := readBounded(b, p, MaxMessageBytes, ErrEncoding, "commit", "message")
	if err != nil {
		return err
	}
	c.Author, c.Message = author, message
	return nil
}

// readBounded reads one length-prefixed string and advances the cursor past it.
//
// Every length-prefixed string in this format is read here — commit
// attribution, and the labels and parameters a wrapped key binds itself to —
// so the bound is applied before the slice is taken exactly once, in one
// place. The sentinel and subject are parameters because the callers report
// into different error families; the parsing is the same parsing.
func readBounded(b []byte, p *int, max int, sentinel error, subject, what string) (string, error) {
	n, adv, err := readUvarint(b[*p:])
	if err != nil {
		return "", fmt.Errorf("%w: %s %s length", sentinel, subject, what)
	}
	*p += adv
	if n > uint64(max) {
		return "", fmt.Errorf("%w: %s %s is %d bytes, above %d", sentinel, subject, what, n, max)
	}
	if uint64(len(b)-*p) < n {
		return "", fmt.Errorf("%w: %s ends inside its %s", sentinel, subject, what)
	}
	s := string(b[*p : *p+int(n)])
	*p += int(n)
	return s, nil
}
