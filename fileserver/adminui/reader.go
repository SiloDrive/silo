package adminui

import "bytes"

// newReader gives http.ServeContent the ReadSeeker it needs. A function rather
// than an inline expression at the one call site, so the reason is written
// down: ServeContent handles ranges and conditional requests, which a plain
// w.Write would not, and a bytes.Reader is the whole cost of getting them.
func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
