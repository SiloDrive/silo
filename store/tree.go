package store

// Shared rules for directory and commit objects: the node types, the bounds
// every parser enforces, and the timestamp range the whole format shares.

// NodeType classifies one edge of the tree. The field is server-consumed, not
// client metadata: GC's mark phase must know whether a child id names a
// manifest or a directory object to continue the walk. That is why it can
// never be sealed or demoted later, and why an unknown value is rejected
// rather than skipped — a marker that cannot classify an edge cannot mark it.
type NodeType uint8

const (
	// NodeFile names a manifest.
	NodeFile NodeType = 0
	// NodeDir names a directory object.
	NodeDir NodeType = 1
	// NodeSymlink names a manifest too — the link's target bytes are the
	// manifest's content, which is the representation this inherited. A client
	// that had nowhere to put a symlink would have to either follow it,
	// duplicating bytes and looping on cycles, or silently drop it; a
	// filesystem may do neither.
	NodeSymlink NodeType = 2
)

func (t NodeType) valid() bool { return t <= NodeSymlink }

// The pinned bounds. They are the format's, not this implementation's: a port
// that accepts more accepts objects another port refuses.
const (
	// MaxDirBytes bounds an encoded directory object, checkable from
	// Content-Length before anything is parsed or allocated. An entry-count
	// cap would not bound allocation — a million entries spans 41 to 225 MB
	// depending on name lengths — and would cap a legitimate structure:
	// million-entry directories exist, and lifting a format ceiling is a
	// version bump.
	MaxDirBytes = 256 << 20
	// MaxCommitBytes bounds an encoded commit.
	MaxCommitBytes = 128 << 10

	// MaxNameBytes bounds one entry's name field, and with it the base64url
	// form an encrypted name takes in entries/{path}: that form is itself a
	// path segment, so it is held to the same ceiling as a plain name. The
	// two E2EE bounds below are what applying it twice comes to.
	MaxNameBytes = 255
	// MaxNameCTBytes bounds an E2EE entry's name ciphertext — the most SIV
	// bytes whose base64url form still fits MaxNameBytes, since
	// ceil(4n/3) <= 255 gives n <= 191.
	MaxNameCTBytes = MaxNameBytes * 3 / 4
	// MaxPlainNameBytes is the longest filename an E2EE library can hold:
	// the ciphertext ceiling less the synthetic IV that SIV prepends. Plain
	// libraries keep the full MaxNameBytes, because their names are not
	// wrapped in anything.
	MaxPlainNameBytes = MaxNameCTBytes - SIVOverhead
	// MaxParents bounds a commit's parent list.
	MaxParents = 16
	// MaxAuthorBytes and MaxMessageBytes bound a commit's two free-text
	// fields.
	MaxAuthorBytes  = 255
	MaxMessageBytes = 65536

	// MaxMode is the largest permission value an entry may carry. File
	// *type* lives in NodeType, never in mode, or two encodings of one fact
	// reappear one field over.
	MaxMode = 0o7777
	// SymlinkMode is what a type-2 entry always carries. Writers emit it and
	// readers reject anything else: symlink permissions are noise the
	// operating systems disagree about, and a varying value would mint
	// divergent ids for identical trees.
	SymlinkMode = 0o777

	// MaxTimestamp is the largest unix second this format can hold — through
	// roughly the year 2514. Writers clamp into [0, MaxTimestamp]; parsers
	// reject anything outside it. Pre-epoch mtimes exist on real disks, which
	// is why the lower clamp is needed at all, and an unbounded
	// server-writable timestamp is otherwise a reliable way to learn which
	// client formats dates defensively.
	MaxTimestamp = 1<<34 - 1

	// DirSaltSize is the per-directory name salt in an E2EE library.
	DirSaltSize = 16
)

// RootMode is the mode a client reports for a library's root directory.
//
// Every node's mtime and mode live in its parent's dirent, and the root has no
// parent — so the root's mtime is its commit's created_at, and its mode is
// this constant. Left unpinned, Go returns zero, Swift returns "now", and
// `ls -ld` on a mount point disagrees with itself across platforms.
const RootMode = 0o755

// clampTimestamp brings a unix second into the range the format can hold.
// Writers clamp; readers reject. Two clamping stories for one data type would
// be a port trap, so there is exactly one, here.
func clampTimestamp(sec int64) int64 {
	if sec < 0 {
		return 0
	}
	if sec > MaxTimestamp {
		return MaxTimestamp
	}
	return sec
}
