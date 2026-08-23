// Package store implements the silo store format: content-defined chunking,
// object ids, the manifest/directory/commit codecs, the content crypto, and
// the key wrapping that gets a client the keys the rest of it needs.
//
// It is the whole of the format, in one package, on purpose. Every id and
// every sealed byte in a library is produced here, so a client that imports
// this package cannot disagree with the server about what a tree hashes to.
// The TUI/CLI and porter-fuse import it; porter-mac reimplements it in Swift
// against the test vectors in testdata/vectors, which are part of the spec
// rather than part of this package's tests. Two implementations that disagree
// by a byte mint different ids for identical trees and fail to read each
// other's libraries, so the vectors are the contract and the spec
// (docs/spec/store-format.md) is its prose.
//
// What is deliberately not here: packs, pack indexes, storage-layer
// encryption, tiering. Those are the server's business — a pack never crosses
// the wire, so no client has any use for the code that writes one.
//
// Nor are the endpoints. The key wrapping here is the primitives and their
// bytes: deriving a password into its two halves, wrapping an identity key,
// wrapping a content key to a member, recovery codes. Serving a pre-login salt
// without turning it into an account-enumeration oracle is a server concern
// and lands with the credential rewrite in docs/auth.md.
package store
