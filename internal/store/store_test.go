package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/gira0/open-pages/internal/names"
)

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	for range 2 {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for table, want := range map[string][]string{
			"user":         {"userid", "email", "password", "oidc_issuer", "oidc_subject"},
			"session":      {"sessionid", "userid", "token", "expires"},
			"api_token":    {"tokenid", "userid", "name", "prefix", "hash", "created", "expires"},
			"groups":       {"groupid", "name", "owner", "name_key"},
			"user_group":   {"ugid", "uid", "gid", "oidc"},
			"docs":         {"docid", "uowner", "ugroup", "name", "description", "path", "visibility"},
			"site_version": {"docid", "version", "bytes"},
		} {
			got := columns(t, db, table)
			for _, c := range want {
				if !got[c] {
					t.Errorf("table %s is missing column %s", table, c)
				}
			}
		}
		var fk int
		if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Errorf("foreign_keys = %d, err %v; want 1", fk, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenError(t *testing.T) {
	// A directory that does not exist makes the first statement fail.
	if db, err := Open(filepath.Join(t.TempDir(), "missing", "data.db")); err == nil {
		_ = db.Close()
		t.Fatal("Open in a missing directory succeeded")
	}
}

// legacySchema is a database from before owners, visibility, OIDC and unique group names.
const legacySchema = `
CREATE TABLE user (userid INTEGER PRIMARY KEY, email VARCHAR(255) NOT NULL, password BINARY(60) NOT NULL);
CREATE TABLE groups (groupid INTEGER PRIMARY KEY, name VARCHAR(255) NOT NULL);
CREATE TABLE user_group (ugid INTEGER PRIMARY KEY, uid INTEGER NOT NULL, gid INTEGER NOT NULL);
CREATE TABLE docs (docid INTEGER PRIMARY KEY, uowner INTEGER NULL, ugroup INTEGER NULL,
	name VARCHAR(256) NOT NULL UNIQUE, description VARCHAR(512) NULL, path VARCHAR(256) NULL UNIQUE);
INSERT INTO groups (name) VALUES ('Eng'), ('eng'), ('Ärzte'), ('ärzte');
INSERT INTO user_group (uid, gid) VALUES (1, 1), (1, 1), (2, 1);
INSERT INTO docs (name, path) VALUES ('old', 'old');
`

func TestOpenMigratesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	var first []string
	for round := range 2 { // the second start must change nothing
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query("SELECT name FROM groups ORDER BY groupid")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		_ = rows.Close()

		if round == 0 {
			first = got
			want := []string{"Eng", "eng-2", "Ärzte", "ärzte-4"}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("group names = %q, want %q", got, want)
				}
			}
			var vis string
			if err := db.QueryRow("SELECT visibility FROM docs WHERE name = 'old'").Scan(&vis); err != nil || vis != "public" {
				t.Errorf("legacy site visibility = %q, err %v; want public", vis, err)
			}
			var members int
			if err := db.QueryRow("SELECT COUNT(*) FROM user_group").Scan(&members); err != nil || members != 2 {
				t.Errorf("memberships = %d, err %v; want duplicates removed (2)", members, err)
			}
			var key string
			if err := db.QueryRow("SELECT name_key FROM groups WHERE groupid = 2").Scan(&key); err != nil || key != names.GroupKey("eng-2") {
				t.Errorf("name_key = %q, err %v", key, err)
			}
		} else {
			for i := range first {
				if got[i] != first[i] {
					t.Fatalf("second migration changed names: %q -> %q", first, got)
				}
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenAddsAuditLogToExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got := columns(t, db, "audit_log")
		for _, c := range []string{"auditid", "time", "actor", "actor_email", "actor_via", "action", "target_kind", "target", "siteid", "groupid", "details", "client_ip"} {
			if !got[c] {
				t.Errorf("audit_log lacks column %s", c)
			}
		}
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM pragma_index_list('audit_log')").Scan(&n); err != nil || n != 3 {
			t.Errorf("audit_log indexes = %d, err %v; want 3", n, err)
		}
		// Entries must outlive the user and the site: no foreign keys, and kinds are checked.
		if _, err := db.Exec(`INSERT INTO audit_log (time, actor, action, target_kind, target, siteid) VALUES (1, 999, 'site.create', 'site', 'gone', 999)`); err != nil {
			t.Errorf("entry for unknown user and site rejected: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO audit_log (time, action, target_kind, target) VALUES (1, 'x', 'user', 'y')`); err == nil {
			t.Error("unknown target_kind accepted")
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSiteVersionFollowsSite(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO docs (docid, name) VALUES (1, 'blog')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO site_version (docid, version, bytes) VALUES (1, 'v1', 10), (1, 'v2', 20)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO site_version (docid, version, bytes) VALUES (1, 'v1', 5)`); err == nil {
		t.Error("duplicate version accepted")
	}
	if _, err := db.Exec(`INSERT INTO site_version (docid, version, bytes) VALUES (9, 'v1', 5)`); err == nil {
		t.Error("version of an unknown site accepted")
	}
	if _, err := db.Exec(`DELETE FROM docs WHERE docid = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM site_version`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows left after deleting the site = %d, err %v", n, err)
	}
}

func TestOpenMakesDatabaseGroupWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o060 != 0o060 {
		t.Errorf("data.db mode = %v, want group read and write", fi.Mode().Perm())
	}
}
