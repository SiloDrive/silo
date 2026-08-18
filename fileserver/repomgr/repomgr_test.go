package repomgr

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/dkam/silo/fileserver/commitmgr"
	_ "github.com/go-sql-driver/mysql"
)

const (
	user            = "seafile"
	password        = "seafile"
	host            = "127.0.0.1"
	port            = 3306
	dbName          = "seafile-db"
	useTLS          = false
	seafileConfPath = "/root/conf"
	seafileDataDir  = "/root/conf/seafile-data"
)

// TestGet runs against a live MySQL deployment holding the repo named by
// TEST_REPO_ID, and skips when there isn't one.
//
// The setup is in the test rather than in a TestMain, which is not style: a
// TestMain that calls os.Exit(0) when the variable is unset skips every test in
// the package, so anything added beside this one would silently never run.
// TestGetWithReason below covers the same function without needing a server.
func TestGet(t *testing.T) {
	repoID := os.Getenv("TEST_REPO_ID")
	if repoID == "" {
		t.Skip("TEST_REPO_ID not set")
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?tls=%t", user, password, host, port, dbName, useTLS)
	seafDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { _ = seafDB.Close() })
	Init(seafDB, seafDB)
	commitmgr.Init(seafileConfPath, seafileDataDir)

	repo := Get(repoID)
	if repo == nil {
		t.Fatalf("failed to get repo : %s", repoID)
	}
	if repo.ID != repoID {
		t.Errorf("got repo %s, want %s", repo.ID, repoID)
	}
}
