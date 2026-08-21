package silod

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("failed to create %s: %v", path, err)
	}
}

// A fresh data directory has nothing to refuse.
func TestCheckLegacyDatabasesAllowsFreshDirectory(t *testing.T) {
	dir := t.TempDir()

	if err := checkLegacyDatabases(dir, filepath.Join(dir, DatabaseName)); err != nil {
		t.Errorf("checkLegacyDatabases rejected a fresh directory: %v", err)
	}
}

// The failure this guards against is silent: opening silo.db would succeed,
// create every table empty, and serve a directory that still holds every user
// and library as though it held none.
func TestCheckLegacyDatabasesRefusesLegacyDirectory(t *testing.T) {
	for _, present := range [][]string{
		{"ccnet.db"},
		{"seafile.db"},
		{"ccnet.db", "seafile.db"},
	} {
		t.Run(strings.Join(present, "+"), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range present {
				touch(t, filepath.Join(dir, name))
			}

			err := checkLegacyDatabases(dir, filepath.Join(dir, DatabaseName))
			if err == nil {
				t.Fatal("checkLegacyDatabases accepted a legacy directory, want an error")
			}
			// The error is the only instruction anyone gets, so it has to
			// carry the recipe rather than just the diagnosis.
			for _, want := range append([]string{"sqlite3", "wal_checkpoint", ".dump", DatabaseName}, present...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n%v", want, err)
				}
			}
			// And only the files that are actually there: naming a file the
			// operator does not have sends them looking for it.
			for _, name := range legacyDatabaseNames {
				if !slices.Contains(present, name) && strings.Contains(err.Error(), name) {
					t.Errorf("error mentions %q, which is not in this directory:\n%v", name, err)
				}
			}
		})
	}
}

// Once the single database exists the old files are history — either already
// folded in or deliberately left behind — and must not block startup.
func TestCheckLegacyDatabasesIgnoresLegacyOnceMerged(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "ccnet.db"))
	touch(t, filepath.Join(dir, "seafile.db"))
	touch(t, filepath.Join(dir, DatabaseName))

	if err := checkLegacyDatabases(dir, filepath.Join(dir, DatabaseName)); err != nil {
		t.Errorf("checkLegacyDatabases rejected a directory that already has %s: %v", DatabaseName, err)
	}
}
