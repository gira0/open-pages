package server

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
	Quota  userQuota   `json:"quota"`
}

// userQuota is the user's storage usage and the configured limits; a limit of 0 means none.
type userQuota struct {
	UsageBytes     int64 `json:"usage_bytes"`      // bytes held by all sites the user owns
	LimitBytes     int64 `json:"limit_bytes"`      // user_mb in bytes
	SiteLimitBytes int64 `json:"site_limit_bytes"` // site_mb in bytes
	Sites          int   `json:"sites"`            // sites the user owns
	SiteLimit      int   `json:"site_limit"`       // sites_per_user
}

type groupInfo struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	OwnerID *int64 `json:"owner_id"` // null for groups without an owner
}

type userDocs struct {
	Owned    []docInfo `json:"owned"`
	Viewable []docInfo `json:"viewable"` // owned docs, login-only docs, and docs of groups the user and the doc owner share
}

type docInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	OwnerID     *int64 `json:"owner_id"`
	GroupID     *int64 `json:"group_id"`
	Visibility  string `json:"visibility"`
	Bytes       int64  `json:"bytes"` // bytes held by the kept versions of the site
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
		`SELECT DISTINCT d.docid, d.name, COALESCE(d.description, ''), d.uowner, d.ugroup, d.visibility,
		   COALESCE((SELECT SUM(v.bytes) FROM site_version v WHERE v.docid = d.docid), 0) FROM docs d
		 WHERE d.uowner = ? OR d.visibility = 'authenticated' OR (d.ugroup IN (SELECT gid FROM user_group WHERE uid = ?)
		   AND EXISTS (SELECT 1 FROM user_group og WHERE og.uid = d.uowner AND og.gid = d.ugroup))
		 ORDER BY d.name`, uid, uid)
	if err != nil {
		internalError(w, "list docs", err)
		return
	}
	defer docRows.Close()
	for docRows.Next() {
		var d docInfo
		var owner, group sql.NullInt64
		if err := docRows.Scan(&d.ID, &d.Name, &d.Description, &owner, &group, &d.Visibility, &d.Bytes); err != nil {
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

	q := &info.Quota
	q.LimitBytes, q.SiteLimitBytes, q.SiteLimit = s.cfg.UserQuota, s.cfg.SiteQuota, s.cfg.SitesPerUser
	if q.UsageBytes, q.Sites, err = s.userUsage(r.Context(), uid); err != nil {
		internalError(w, "read usage", err)
		return
	}

	writeJSON(w, http.StatusOK, info)
}
