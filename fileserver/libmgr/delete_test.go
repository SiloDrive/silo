package libmgr

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// garbageRowIsExpected names the one table a deleted library is supposed to
// still have a row in. GarbageLibraries is the queue GC reads to find stores to
// reclaim; a delete that did not leave a row there would leak the objects
// instead of the row.
var garbageRowIsExpected = map[string]bool{"GarbageLibraries": true}

// Deleting a library leaves nothing of it behind in any table keyed by its id.
//
// Written against the live schema rather than against a list of table names,
// because the failure this catches is a table somebody adds and forgets to
// delete from -- and a test that names the tables it knows about cannot catch
// the one nobody thought of. Every table with a library_id column is asked, so
// the next per-library table is covered on the day it is created rather than on
// the day somebody notices.
func TestDeleteLibraryLeavesNoRowsBehind(t *testing.T) {
	getTestStore(t)

	libraryID, err := CreateLibrary("Doomed", testAccount(t), DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	// Fill in the tables CreateLibrary does not write, so that "no rows" is a
	// fact about deletion rather than a fact about the row never existing.
	if err := SetRetentionDays(libraryID, 30); err != nil {
		t.Fatalf("SetRetentionDays: %v", err)
	}
	for _, stmt := range []string{
		"INSERT OR REPLACE INTO GCID (library_id, gc_id) VALUES (?, 'abcdef')",
		"INSERT INTO LastGCID (library_id, client_id, gc_id) VALUES (?, 'a-client', 'abcdef')",
		"INSERT INTO LibraryValidSince (library_id, timestamp) VALUES (?, 1)",
		"INSERT INTO LibraryHistoryLimit (library_id, days) VALUES (?, 7)",
	} {
		if _, err := writeDB.Exec(stmt, libraryID); err != nil {
			t.Fatalf("seeding a row: %v", err)
		}
	}

	if err := DeleteLibrary(libraryID); err != nil {
		t.Fatalf("DeleteLibrary: %v", err)
	}

	tables, err := tablesKeyedByLibrary()
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 10 {
		t.Fatalf("only found %d library-keyed tables (%v); the schema query is wrong, "+
			"and a test that asks nothing passes for the wrong reason", len(tables), tables)
	}

	for _, table := range tables {
		if garbageRowIsExpected[table] {
			continue
		}
		var n int
		q := fmt.Sprintf("SELECT COUNT(*) FROM %q WHERE library_id = ?", table)
		if err := readDB.QueryRow(q, libraryID).Scan(&n); err != nil {
			t.Errorf("counting %s: %v", table, err)
			continue
		}
		if n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted library; "+
				"DeleteLibrary does not clear it", table, n)
		}
	}
}

// tablesKeyedByLibrary is every table in the schema with a library_id column.
func tablesKeyedByLibrary() ([]string, error) {
	ctx := context.Background()
	rows, err := readDB.QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var keyed []string
	for _, name := range names {
		cols, err := readDB.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", name))
		if err != nil {
			return nil, fmt.Errorf("reading columns of %s: %w", name, err)
		}
		has := false
		for cols.Next() {
			var (
				cid        int
				colName    string
				colType    string
				notNull    int
				defaultVal any
				pk         int
			)
			if err := cols.Scan(&cid, &colName, &colType, &notNull, &defaultVal, &pk); err != nil {
				_ = cols.Close()
				return nil, err
			}
			if colName == "library_id" {
				has = true
			}
		}
		err = cols.Err()
		_ = cols.Close()
		if err != nil {
			return nil, err
		}
		if has {
			keyed = append(keyed, name)
		}
	}
	sort.Strings(keyed)
	return keyed, nil
}
