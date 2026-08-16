package silod

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

// RunBackupDB snapshots the SQLite databases into a destination directory.
//
// The databases are the part of a Silo backup that cannot be copied with cp.
// They run in WAL mode, so the bytes in seafile.db lag the committed state by
// however much sits in seafile.db-wal, and Silo only checkpoints on a clean
// shutdown. Copying just the .db file off a running server therefore loses
// every write since the last checkpoint, silently and without any error to
// notice — and copying the -wal and -shm files alongside it is not a fix,
// because the three are read at different instants and need not agree.
//
// VACUUM INTO takes a read transaction over the source and writes a single
// consistent, already-checkpointed file. It is safe while the server is
// running and blocks no writer.
//
// The object store is not copied here. It is an ordinary immutable file tree
// that rsync handles better than anything worth writing, but the *order*
// matters and is the other half of what makes a naive backup wrong:
//
//	databases first, object store second.
//
// Objects are content-addressed and never rewritten, so every object a head
// captured at T1 references is still present at T2. Copy the store first and
// the database after, and the heads can reference objects created in between —
// they are missing from the backup, and the repository restores broken. The
// one thing that can invalidate the rule is gc -delete removing objects
// mid-backup, so do not run GC and a backup together.
func RunBackupDB(args []string) error {
	flags := flag.NewFlagSet("silo backup-db", flag.ContinueOnError)
	flags.StringVar(&configFile, "C", "", "path to config file (optional)")
	flags.StringVar(&dataDir, "d", "", "data directory (default: $SILO_DATA_DIR or ~/.local/share/silo)")
	force := flags.Bool("f", false, "overwrite existing files in the destination")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	rest := flags.Args()
	if err := rejectTrailingFlags("backup-db", rest); err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: silo backup-db [-d datadir] [-C config] [-f] <destination-dir>")
	}
	destDir, err := filepath.Abs(rest[0])
	if err != nil {
		return fmt.Errorf("failed to resolve destination %s: %v", rest[0], err)
	}

	if err := resolvePaths(); err != nil {
		return err
	}

	dbOpt, err := option.LoadDBOption(configFile)
	if err != nil {
		return fmt.Errorf("failed to load database configuration: %v", err)
	}
	if dbOpt.DBEngine != dbutil.EngineSQLite {
		return fmt.Errorf("backup-db only handles SQLite; this deployment uses %s, "+
			"so back it up with that engine's own tooling (mysqldump, pg_dump)", dbOpt.DBEngine)
	}

	if destDir == absDataDir {
		return fmt.Errorf("destination %s is the data directory itself", destDir)
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("failed to create destination %s: %v", destDir, err)
	}

	for _, name := range []string{"ccnet.db", "seafile.db"} {
		src := filepath.Join(absDataDir, name)
		dst := filepath.Join(destDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("cannot read %s: %v", src, err)
		}
		// VACUUM INTO refuses to overwrite, which is the behaviour we want by
		// default: a backup that silently replaced last night's good copy with
		// a failed one would be worse than no backup. -f is the explicit
		// opt-out for scripted rotation into a fixed path.
		if _, err := os.Stat(dst); err == nil {
			if !*force {
				return fmt.Errorf("%s already exists; use -f to overwrite", dst)
			}
			if err := os.Remove(dst); err != nil {
				return fmt.Errorf("failed to remove %s: %v", dst, err)
			}
		}
		if err := vacuumInto(src, dst); err != nil {
			return err
		}
		info, err := os.Stat(dst)
		if err != nil {
			return fmt.Errorf("failed to stat %s: %v", dst, err)
		}
		fmt.Printf("%s → %s (%s)\n", src, dst, humanBytes(info.Size()))
	}

	storeDir := filepath.Join(absDataDir, "storage")
	fmt.Printf(`
Databases copied. Now copy the object store, in this order — never before:

  rsync -a %s/ %s/storage/

Objects are immutable, so anything the copied heads reference is still there.
Copying the store first and the databases after can capture heads that point
at objects the backup does not contain. Do not run "silo gc -delete" while a
backup is in progress.
`, storeDir, destDir)

	return nil
}

// rejectTrailingFlags fails on a flag that appears after the first positional
// argument. flag.Parse stops parsing there and hands the rest back as
// positionals, so "-d /srv/silo" written at the end is not a parse error — it
// is silently ignored, and the command runs against the default data
// directory. For a command that reports what a user's tokens are, or writes a
// backup, being pointed at the wrong deployment without saying so is worse
// than refusing.
func rejectTrailingFlags(cmd string, rest []string) error {
	for _, arg := range rest {
		if len(arg) > 1 && arg[0] == '-' {
			return fmt.Errorf("flag %s must come before the arguments: silo %s %s ...", arg, cmd, arg)
		}
	}
	return nil
}

// vacuumInto writes a consistent snapshot of a live SQLite database to dst.
//
// It opens its own connection rather than reusing dbutil.OpenSQLite: that
// helper sets query_only on the read handle and runs schema creation on the
// write handle, and a backup should neither be rejected as a write nor be
// able to modify the database it is copying.
func vacuumInto(src, dst string) error {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout%%3D30000", src)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open %s: %v", src, err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		// Leave nothing half-written behind for the next run to mistake for a
		// good backup.
		_ = os.Remove(dst)
		return fmt.Errorf("failed to snapshot %s into %s: %v", src, dst, err)
	}
	return nil
}
