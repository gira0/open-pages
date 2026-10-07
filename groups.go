package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Groups. Any logged-in user may create a group and becomes its owner and first member.
// Only the owner can delete the group or add and remove other members; any member can
// leave. The owner can't be removed: delete the group instead. A group that still has
// sites (docs.ugroup) can't be deleted, so a site never silently loses its group (which,
// once access control exists, could widen who can see it). Groups created before owners
// existed have no owner and can't be changed through the API.
//
// Helpers for other code, such as per-site access control: userInGroup, userGroups,
// getGroup and groupMembers. They only read; all writes below are single statements
// or transactions that check authorization in SQL, so a stale read can't be acted on.

const maxGroupNameLen = 64

var (
	errGroupExists    = errors.New("a group with that name already exists")
	errGroupInUse     = errors.New("group is still used by sites; move or delete them first")
	errNotGroupOwner  = errors.New("you do not own this group")
	errNotGroupMember = errors.New("you are not a member of this group")
	errUserNotFound   = errors.New("user not found")
	errOwnerMember    = errors.New("the owner can't be removed from the group; delete the group instead")
	errNotAMember     = errors.New("user is not a member of this group")
)

// Group is a named set of users.
type Group struct {
	ID      int64
	Name    string
	OwnerID int64 // 0 when the group has no owner
}

type memberInfo struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

// validGroupName trims name and reports whether it is usable: 1 to maxGroupNameLen
// characters, no control characters.
func validGroupName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	n := utf8.RuneCountInString(name)
	if n == 0 || n > maxGroupNameLen || !utf8.ValidString(name) {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// userInGroup reports whether the user is a member of the group.
func (s *Server) userInGroup(ctx context.Context, userID, groupID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM user_group WHERE uid = ? AND gid = ?)", userID, groupID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("query membership: %w", err)
	}
	return ok, nil
}

// userGroups lists the groups the user belongs to, ordered by name. It is never nil.
func (s *Server) userGroups(ctx context.Context, userID int64) ([]groupInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.groupid, g.name, g.owner FROM groups g
		 WHERE g.groupid IN (SELECT gid FROM user_group WHERE uid = ?)
		 ORDER BY g.name, g.groupid`, userID)
	if err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	defer rows.Close()
	out := []groupInfo{}
	for rows.Next() {
		var (
			g     groupInfo
			owner sql.NullInt64
		)
		if err := rows.Scan(&g.ID, &g.Name, &owner); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		if owner.Valid {
			g.OwnerID = &owner.Int64
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	return out, nil
}

// getGroup loads a group by id. It returns errGroupNotFound if there is none.
func (s *Server) getGroup(ctx context.Context, id int64) (Group, error) {
	g := Group{ID: id}
	var owner sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT name, owner FROM groups WHERE groupid = ?", id).Scan(&g.Name, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, errGroupNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("query group: %w", err)
	}
	g.OwnerID = owner.Int64
	return g, nil
}

// groupMembers lists the group's members ordered by email. It is never nil.
func (s *Server) groupMembers(ctx context.Context, groupID int64) ([]memberInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.userid, u.email FROM user_group ug JOIN user u ON u.userid = ug.uid
		 WHERE ug.gid = ? ORDER BY u.email, u.userid`, groupID)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	defer rows.Close()
	out := []memberInfo{}
	for rows.Next() {
		var m memberInfo
		if err := rows.Scan(&m.ID, &m.Email); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	return out, nil
}

// createGroup makes a group owned by owner, who is also its first member. The name must
// already be validated. It returns errGroupExists if the name is taken (ignoring case).
func (s *Server) createGroup(ctx context.Context, name string, owner int64) (Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "INSERT INTO groups (name, owner) VALUES (?, ?) ON CONFLICT DO NOTHING", name, owner)
	if err != nil {
		return Group{}, fmt.Errorf("insert group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Group{}, errGroupExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Group{}, fmt.Errorf("group id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO user_group (uid, gid) VALUES (?, ?)", owner, id); err != nil {
		return Group{}, fmt.Errorf("insert owner membership: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Group{}, fmt.Errorf("commit: %w", err)
	}
	return Group{ID: id, Name: name, OwnerID: owner}, nil
}

// deleteGroup deletes the group and its memberships if actor owns it and no site uses
// it. Ownership and "unused" are checked inside the DELETE statements, in one
// transaction, so a concurrent site update or ownership change can't slip in between.
// It returns errGroupNotFound, errNotGroupOwner or errGroupInUse otherwise.
func (s *Server) deleteGroup(ctx context.Context, actor, id int64) error {
	const guard = ` AND EXISTS (SELECT 1 FROM groups WHERE groupid = ? AND owner = ?)
		AND NOT EXISTS (SELECT 1 FROM docs WHERE ugroup = ?)`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM user_group WHERE gid = ?"+guard, id, id, actor, id); err != nil {
		return fmt.Errorf("delete memberships: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM groups WHERE groupid = ?"+guard, id, id, actor, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		return nil
	}
	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	// Nothing was deleted: say why.
	g, err := s.getGroup(ctx, id)
	if err != nil {
		return err
	}
	if g.OwnerID == 0 || g.OwnerID != actor {
		return errNotGroupOwner
	}
	return errGroupInUse
}

// addGroupMember adds the user with the given email to the group if actor owns it, in a
// single statement. added is false if the user was already a member. It returns
// errGroupNotFound, errNotGroupOwner or errUserNotFound otherwise.
func (s *Server) addGroupMember(ctx context.Context, actor, groupID int64, email string) (added bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user_group (uid, gid)
		 SELECT u.userid, g.groupid FROM user u, groups g
		 WHERE u.email = ? AND g.groupid = ? AND g.owner = ?
		 ON CONFLICT DO NOTHING`, email, groupID, actor)
	if err != nil {
		return false, fmt.Errorf("add member: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// Nothing inserted: not allowed, user unknown, or already a member.
	g, err := s.getGroup(ctx, groupID)
	if err != nil {
		return false, err
	}
	if g.OwnerID == 0 || g.OwnerID != actor {
		return false, errNotGroupOwner
	}
	var one int
	err = s.db.QueryRowContext(ctx, "SELECT 1 FROM user WHERE email = ?", email).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("query user: %w", err)
	}
	return false, nil
}

// removeGroupMember removes target from the group. The owner may remove any other
// member and a member may remove themselves; the owner itself can't be removed. The
// rules are checked inside the DELETE. It returns errGroupNotFound, errNotGroupOwner
// (actor may not remove target), errOwnerMember or errNotAMember otherwise.
func (s *Server) removeGroupMember(ctx context.Context, actor, groupID, target int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM user_group WHERE gid = ? AND uid = ?
		 AND uid IS NOT (SELECT owner FROM groups WHERE groupid = ?)
		 AND (uid = ? OR ? IS (SELECT owner FROM groups WHERE groupid = ?))`,
		groupID, target, groupID, actor, actor, groupID)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	g, err := s.getGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if g.OwnerID != 0 && g.OwnerID == target {
		return errOwnerMember
	}
	if actor != target && (g.OwnerID == 0 || g.OwnerID != actor) {
		return errNotGroupOwner
	}
	return errNotAMember
}

// idParam parses the named path value as a positive integer id. ok is false (and a 404
// is written) if it is not one.
func idParam(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, errGroupNotFound.Error())
		return 0, false
	}
	return id, true
}

// groupError writes the response for an error from the group helpers and reports
// whether err was non-nil.
func groupError(w http.ResponseWriter, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errGroupNotFound), errors.Is(err, errUserNotFound), errors.Is(err, errNotAMember):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errNotGroupOwner), errors.Is(err, errNotGroupMember):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, errGroupExists), errors.Is(err, errGroupInUse):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errOwnerMember):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		internalError(w, what, err)
	}
	return true
}

// handleGroupCreate: POST /v1/auth/groups {"name": "..."}.
func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var d struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name, ok := validGroupName(d.Name)
	if !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("name must be 1 to %d characters without control characters", maxGroupNameLen))
		return
	}
	g, err := s.createGroup(r.Context(), name, userID(r))
	if groupError(w, "create group", err) {
		return
	}
	writeJSON(w, http.StatusCreated, groupInfo{ID: g.ID, Name: g.Name, OwnerID: &g.OwnerID})
}

// handleGroupList: GET /v1/auth/groups lists the caller's groups.
func (s *Server) handleGroupList(w http.ResponseWriter, r *http.Request) {
	groups, err := s.userGroups(r.Context(), userID(r))
	if groupError(w, "list groups", err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// handleGroupGet: GET /v1/auth/groups/{id} returns the group and its members. Members only.
func (s *Server) handleGroupGet(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	g, err := s.getGroup(r.Context(), id)
	if groupError(w, "load group", err) {
		return
	}
	member, err := s.userInGroup(r.Context(), userID(r), id)
	if groupError(w, "check membership", err) {
		return
	}
	if !member {
		groupError(w, "check membership", errNotGroupMember)
		return
	}
	members, err := s.groupMembers(r.Context(), id)
	if groupError(w, "list members", err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": g.ID, "name": g.Name, "owner_id": nullableID(g.OwnerID), "members": members,
	})
}

// handleGroupDelete: DELETE /v1/auth/groups/{id}. Owner only.
func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	if groupError(w, "delete group", s.deleteGroup(r.Context(), userID(r), id)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "group deleted", "id": id})
}

// handleMemberAdd: POST /v1/auth/groups/{id}/members {"email": "..."}. Owner only.
func (s *Server) handleMemberAdd(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	var d struct {
		Email string `json:"email"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Email == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with an email")
		return
	}
	added, err := s.addGroupMember(r.Context(), userID(r), id, d.Email)
	if groupError(w, "add member", err) {
		return
	}
	status, msg := http.StatusOK, "already a member"
	if added {
		status, msg = http.StatusCreated, "member added"
	}
	writeJSON(w, status, map[string]any{"status": msg, "group": id})
}

// handleMemberRemove: DELETE /v1/auth/groups/{id}/members/{uid}. Owner, or the member
// themselves to leave.
func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	target, ok := idParam(w, r, "uid")
	if !ok {
		return
	}
	if groupError(w, "remove member", s.removeGroupMember(r.Context(), userID(r), id, target)) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "member removed", "group": id, "user": target})
}
