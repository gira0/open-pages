package main

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS user (
	userid   INTEGER PRIMARY KEY,
	email    VARCHAR(255) NOT NULL,
	password BINARY(60) NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS user_email ON user (email);

CREATE TABLE IF NOT EXISTS session (
	sessionid INTEGER PRIMARY KEY,
	userid    INTEGER NOT NULL,
	token     CHAR(512) NOT NULL UNIQUE,
	expires   BIGINT NOT NULL,
	FOREIGN KEY (userid) REFERENCES user(userid)
);

CREATE TABLE IF NOT EXISTS api_token (
	tokenid INTEGER PRIMARY KEY,
	userid  INTEGER NOT NULL,
	name    VARCHAR(64) NOT NULL,
	prefix  VARCHAR(16) NOT NULL,
	hash    CHAR(64) NOT NULL UNIQUE,
	created BIGINT NOT NULL,
	expires BIGINT NULL,
	FOREIGN KEY (userid) REFERENCES user(userid)
);

CREATE INDEX IF NOT EXISTS api_token_user ON api_token (userid);

CREATE TABLE IF NOT EXISTS groups (
	groupid  INTEGER PRIMARY KEY,
	name     VARCHAR(255) NOT NULL,
	owner    INTEGER NULL,
	name_key TEXT NULL,
	FOREIGN KEY (owner) REFERENCES user(userid)
);

CREATE TABLE IF NOT EXISTS user_group (
	ugid INTEGER PRIMARY KEY,
	uid  INTEGER NOT NULL,
	gid  INTEGER NOT NULL,
	FOREIGN KEY (uid) REFERENCES user(userid),
	FOREIGN KEY (gid) REFERENCES groups(groupid)
);

CREATE TABLE IF NOT EXISTS docs (
	docid       INTEGER PRIMARY KEY,
	uowner      INTEGER NULL,
	ugroup      INTEGER NULL,
	name        VARCHAR(256) NOT NULL UNIQUE,
	description VARCHAR(512) NULL,
	path        VARCHAR(256) NULL UNIQUE,
	visibility  TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'restricted')),
	FOREIGN KEY (uowner) REFERENCES user(userid),
	FOREIGN KEY (ugroup) REFERENCES groups(groupid)
);
`

// openDB opens (creating if needed) the SQLite database at path and applies the schema.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return db, nil
}

// migrate brings databases created by older versions up to date. Every step is
// idempotent, so it runs on each start.
func migrate(db *sql.DB) error {
	ctx := context.Background()
	// groups.owner: the user who administers the group. Groups that predate it keep a
	// NULL owner and can't be changed through the API until an operator sets one.
	// groups.name_key: the case-folded name that carries the uniqueness (see groupNameKey).
	for _, col := range []struct{ name, ddl string }{
		{"owner", "owner INTEGER NULL REFERENCES user(userid)"},
		{"name_key", "name_key TEXT NULL"},
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = ?", col.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.ExecContext(ctx, "ALTER TABLE groups ADD COLUMN "+col.ddl); err != nil {
				return err
			}
		}
	}
	// docs.visibility: who may see a site (public or restricted); existing sites stay public.
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('docs') WHERE name = 'visibility'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE docs ADD COLUMN visibility TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'restricted'))"); err != nil {
			return err
		}
	}
	if err := migrateGroupNames(ctx, db); err != nil {
		return fmt.Errorf("group names: %w", err)
	}
	// A user is in a group at most once, and group names are unique ignoring case.
	steps := []string{
		"DELETE FROM user_group WHERE ugid NOT IN (SELECT MIN(ugid) FROM user_group GROUP BY uid, gid)",
		"CREATE UNIQUE INDEX IF NOT EXISTS user_group_member ON user_group (uid, gid)",
		"DROP INDEX IF EXISTS groups_name",
		"CREATE UNIQUE INDEX IF NOT EXISTS groups_name_key ON groups (name_key)",
	}
	for _, q := range steps {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// migrateGroupNames fills groups.name_key and resolves names that only differ in case.
// The group with the lowest id keeps its name; each later one gets "-<id>" appended
// (shortening the name so it still fits maxGroupNameLen). Rows already in order are
// left alone, so running it again changes nothing.
func migrateGroupNames(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	type row struct {
		id        int64
		name      string
		storedKey sql.NullString
	}
	rows, err := tx.QueryContext(ctx, "SELECT groupid, name, name_key FROM groups ORDER BY groupid")
	if err != nil {
		return err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.storedKey); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(all))
	for _, r := range all {
		name, key := r.name, groupNameKey(r.name)
		for suffix := fmt.Sprintf("-%d", r.id); seen[key]; suffix += "_" {
			base := []rune(r.name)
			if keep := maxGroupNameLen - len([]rune(suffix)); len(base) > keep {
				base = base[:keep]
			}
			name = string(base) + suffix
			key = groupNameKey(name)
		}
		seen[key] = true
		if name == r.name && r.storedKey.Valid && r.storedKey.String == key {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE groups SET name = ?, name_key = ? WHERE groupid = ?", name, key, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
