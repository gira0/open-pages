package main

import (
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
	name    VARCHAR(255) NOT NULL
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return db, nil
}
