package main

import (
	"database/sql"
	"errors"
	"net/http"
)

// userInfo is the response of GET /v1/auth/user. Add new sections as new
// fields so existing clients keep working. It never carries the password hash.
type userInfo struct {
	UserID int64       `json:"userid"` // kept for clients of the original endpoint
	Email  string      `json:"email"`
	Groups []groupInfo `json:"groups"`
	Docs   userDocs    `json:"docs"`
}

type groupInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	OwnerID *int64 `json:"owner_id"` // null for groups without an owner
}

type userDocs struct {
	Owned    []docInfo `json:"owned"`
	Viewable []docInfo `json:"viewable"` // owned docs plus docs of the user's groups
}

type docInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	OwnerID     *int64 `json:"owner_id"`
	GroupID     *int64 `json:"group_id"`
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	info := userInfo{UserID: uid, Groups: []groupInfo{}, Docs: userDocs{Owned: []docInfo{}, Viewable: []docInfo{}}}

	err := s.db.QueryRowContext(r.Context(), "SELECT email FROM user WHERE userid = ?", uid).Scan(&info.Email)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if err != nil {
		internalError(w, "lookup user", err)
		return
	}

	groups, err := s.userGroups(r.Context(), uid)
	if err != nil {
		internalError(w, "list groups", err)
		return
	}
	info.Groups = groups

	// DISTINCT guards against a duplicate user_group row repeating a doc.
	docRows, err := s.db.QueryContext(r.Context(),
		`SELECT DISTINCT d.docid, d.name, COALESCE(d.description, ''), d.uowner, d.ugroup FROM docs d
		 WHERE d.uowner = ? OR d.ugroup IN (SELECT gid FROM user_group WHERE uid = ?)
		 ORDER BY d.name`, uid, uid)
	if err != nil {
		internalError(w, "list docs", err)
		return
	}
	defer docRows.Close()
	for docRows.Next() {
		var d docInfo
		var owner, group sql.NullInt64
		if err := docRows.Scan(&d.ID, &d.Name, &d.Description, &owner, &group); err != nil {
			internalError(w, "scan doc", err)
			return
		}
		if owner.Valid {
			d.OwnerID = &owner.Int64
		}
		if group.Valid {
			d.GroupID = &group.Int64
		}
		info.Docs.Viewable = append(info.Docs.Viewable, d)
		if owner.Valid && owner.Int64 == uid {
			info.Docs.Owned = append(info.Docs.Owned, d)
		}
	}
	if err := docRows.Err(); err != nil {
		internalError(w, "list docs", err)
		return
	}

	writeJSON(w, http.StatusOK, info)
}
