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
	if len(ck) == 0 {
		return nil, fmt.Errorf("store: sealing a commit needs a content key")
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
	if len(ck) == 0 {
		return nil, fmt.Errorf("store: opening a commit needs a content key")
	}
	return decodeCommit(b, ck)
}

func decodeCommit(b, ck []byte) (*Commit, error) {
	if len(b) > MaxCommitBytes {
		return nil, fmt.Errorf("%w: commit is %d bytes, above the %d ceiling",
			ErrEncoding, len(b), MaxCommitBytes)
	}
	if len(b) < 2+IDSize+1 {
		return nil, fmt.Errorf("%w: commit is %d bytes, too short for a header", ErrEncoding, len(b))
	}
	if b[0] != CommitVersion {
		return nil, fmt.Errorf("%w: commit version %d, this build writes %d",
			ErrEncoding, b[0], CommitVersion)
	}
	flags := b[1]
	if flags&^byte(flagE2EE) != 0 {
		return nil, fmt.Errorf("%w: commit reserved flag bits are set (%#02x)", ErrEncoding, flags)
	}
	e2ee := ck != nil
	if (flags&flagE2EE != 0) != e2ee {
		return nil, fmt.Errorf("%w: commit declares E2EE=%t, library is E2EE=%t",
			ErrEncoding, flags&flagE2EE != 0, e2ee)
	}

	c := &Commit{}
	p := 2
	copy(c.Root[:], b[p:p+IDSize])
	p += IDSize

	count, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, fmt.Errorf("%w: commit parent count", ErrEncoding)
	}
	p += n
	if count > MaxParents {
		return nil, fmt.Errorf("%w: commit claims %d parents, above %d", ErrEncoding, count, MaxParents)
	}
	if uint64(len(b)-p) < count*IDSize {
		return nil, fmt.Errorf("%w: commit ends inside its parent list", ErrEncoding)
	}
	c.Parents = make([]ID, count)
	for i := range c.Parents {
		copy(c.Parents[i][:], b[p:])
		p += IDSize
	}

	created, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, fmt.Errorf("%w: commit timestamp", ErrEncoding)
	}
	p += n
	if created > MaxTimestamp {
		return nil, fmt.Errorf("%w: commit timestamp %d above the %d ceiling",
			ErrEncoding, created, uint64(MaxTimestamp))
	}
	c.CreatedAt = int64(created)

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
	author, err := readBounded(b, p, MaxAuthorBytes, "author")
	if err != nil {
		return err
	}
	message, err := readBounded(b, p, MaxMessageBytes, "message")
	if err != nil {
		return err
	}
	c.Author, c.Message = author, message
	return nil
}

func readBounded(b []byte, p *int, max int, what string) (string, error) {
	n, adv, err := readUvarint(b[*p:])
	if err != nil {
		return "", fmt.Errorf("%w: commit %s length", ErrEncoding, what)
	}
	*p += adv
	if n > uint64(max) {
		return "", fmt.Errorf("%w: commit %s is %d bytes, above %d", ErrEncoding, what, n, max)
	}
	if uint64(len(b)-*p) < n {
		return "", fmt.Errorf("%w: commit ends inside its %s", ErrEncoding, what)
	}
	s := string(b[*p : *p+int(n)])
	*p += int(n)
	return s, nil
}
