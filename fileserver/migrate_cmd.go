package silod

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/SiloDrive/silo/fileserver/dbutil"
	"github.com/SiloDrive/silo/fileserver/objstore"
)

// RunMigrate brings silo.db to the shape this build expects, without starting
// the server. The server does the same thing on start; this exists for the
// operator who wants to watch it succeed before opening the port, and for
// -n, which says what would run without running it.
//
// It takes the data directory lock for the same reason the server does: a
// migration is a change to the shape of tables another process may be
// mid-statement on, and the lock is what says no such process exists.
func RunMigrate(args []string) error {
	flags := commandFlags("migrate")
	dry := flags.Bool("n", false, "list the migrations that would run, without running them")
	rest, done, err := parseCommandArgs("migrate", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage:\n" +
			"  silo migrate [-n]   bring the database to this build's schema; -n only lists what would run")
	}
	if err := resolvePaths(); err != nil {
		return err
	}
	lock, err := objstore.LockDataDir(absDataDir)
	if err != nil {
		if errors.Is(err, objstore.ErrDataDirLocked) {
			return fmt.Errorf("%w\nStop the server before running migrate", err)
		}
		return err
	}
	defer func() { _ = lock.Release() }()

	pair, err := dbutil.OpenSQLite(filepath.Join(absDataDir, DatabaseName))
	if err != nil {
		return fmt.Errorf("failed to open database: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if *dry {
		pending, err := dbutil.Pending(pair.Write)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("nothing to migrate")
			return nil
		}
		for _, name := range pending {
			fmt.Println(name)
		}
		return nil
	}

	applied, err := dbutil.Migrate(pair.Write)
	for _, name := range applied {
		fmt.Println("applied", name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("nothing to migrate")
	}
	return nil
}
