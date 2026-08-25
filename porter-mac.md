The defect: the all-zeros sentinel no longer exists

  The brief still says:

  ▎ The all-zeros id is the id of an empty object, not an error. Both an empty directory and a zero-byte file carry it — ids are content hashes and empty content hashes to one value — so it says nothing about which one you
  ▎ have. Read type for that, and do not use the id to tell them apart or as a cache key: a content-addressed cache keyed on it collides every empty object in the account onto one entry.

  Every clause of that is now false. Live:

  dir   emptydir       fb50dc0717ff266cf9baf82b1ce7a1c2ef6d9247859680b11a19fb7077f5f222
  file  zerobyte.txt   d7b3d4012540102c40a23acdeee417e06a42a74a5d66c7efe59f4e4aa0537c5c

  Real, distinct hashes. Under store-v2 the objects are typed, so an empty directory and a zero-byte file hash differently — which means the collision the paragraph warns about cannot happen, and keying a cache on the id is
  now safe rather than forbidden. It's the same failure mode c4f9827 was written about: a correction landed in one half of the file and the other half went on describing the world it replaced. Worth telling silo-claude,
  since it's a warning that now steers a client away from something correct.

  Two smaller ones in the same section: the listing example shows "modifier":"d@nmilne.com" on file entries, and live rows carry only id, mtime, name, size, type — no modifier. And the empty directory's id is the same value
  as the empty root listing's ETag, which is consistent rather than wrong, but it means an implementation can't treat "id equals the root ETag" as meaningful.

  For Porter specifically, this makes SiloModels.swift:97 — emptyDirectoryID = String(repeating: "0", count: 40) — not merely the wrong width but the wrong idea. It should be deleted, not widened to 64.
