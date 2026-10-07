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

CREATE TABLE IF NOT EXISTS groups (
	groupid INTEGER PRIMARY KEY,
	name    VARCHAR(255) NOT NULL,
	owner   INTEGER NULL,
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
	var hasOwner int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'owner'").Scan(&hasOwner); err != nil {
		return err
	}
	if hasOwner == 0 {
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE groups ADD COLUMN owner INTEGER NULL REFERENCES user(userid)"); err != nil {
			return err
		}
	}
	// A user is in a group at most once, and group names are unique ignoring case.
	steps := []string{
		"DELETE FROM user_group WHERE ugid NOT IN (SELECT MIN(ugid) FROM user_group GROUP BY uid, gid)",
		"CREATE UNIQUE INDEX IF NOT EXISTS user_group_member ON user_group (uid, gid)",
		"CREATE UNIQUE INDEX IF NOT EXISTS groups_name ON groups (name COLLATE NOCASE)",
	}
	for _, q := range steps {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
