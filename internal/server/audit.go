package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The audit log is a durable record of who changed which site, group or API token, and
// when: one row in audit_log per successful change. Rows hold snapshots (the actor's email,
// the target's name) so they stay meaningful after the user, site or group is gone, and the
// actor column has no foreign key for the same reason. Every entry is also written to the
// log as an "audit" line, so it reaches log shippers even if the database is lost.
//
// Entries are written after the change succeeded, inside the change's transaction where
// there is one. A failed insert is logged and never fails the request.

// Audit actions.
const (
	auditSiteCreate   = "site.create"
	auditSiteUpdate   = "site.update"
	auditSiteDeploy   = "site.deploy"
	auditSiteRollback = "site.rollback"
	auditSiteDelete   = "site.delete"
	auditGroupCreate  = "group.create"
	auditGroupDelete  = "group.delete"
	auditMemberAdd    = "group.member_add"
	auditMemberRemove = "group.member_remove"
	auditTokenCreate  = "token.create"
	auditTokenRevoke  = "token.revoke"
)

// Audit target kinds.
const (
	targetSite  = "site"
	targetGroup = "group"
	targetToken = "token"
)

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 500
	auditPruneEvery   = 24 * time.Hour
)

type auditSourceKey struct{}

// auditSource says how and from where a request was authenticated.
type auditSource struct {
	via string // "session" or "token:<display prefix of the API token>"
	ip  string
}

// viaSession is the audit credential of a session cookie; an API token is "token:<prefix>".
const viaSession = "session"

// viaOf names the credential requireAuth accepted for r.
func viaOf(r *http.Request) string {
	if token, ok := bearerToken(r); ok {
		// The display prefix is what the token list shows; it identifies a token without
		// revealing it.
		return "token:" + token[:min(len(token), len(tokenPrefix)+tokenShownChars)]
	}
	return viaSession
}

// withAuditSource records the credential and client address of r in its context, for
// audit entries written further down. Call it once the request is authenticated.
func withAuditSource(r *http.Request, via string) context.Context {
	return context.WithValue(r.Context(), auditSourceKey{}, auditSource{via: via, ip: clientIP(r)})
}

// clientIP is the host part of the peer address. There is no trusted-proxy support yet, so
// behind a reverse proxy this is the proxy's address; X-Forwarded-For handling arrives with #72.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// auditEvent is one thing that happened. The actor, credential and address come from the
// request context.
type auditEvent struct {
	action     string
	targetKind string
	target     string         // name of the site, group or token, as it is now
	siteID     int64          // 0 when not about a site
	groupID    int64          // 0 when not about a group
	details    map[string]any // small, JSON-encodable; nil for none
}

// execQuerier is the part of *sql.DB and *sql.Tx that audit needs.
type execQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// audit records ev, attributed to the user in ctx, and logs it. q is the database. Call it
// while still holding whatever lock made the change safe (see handleSiteAudit for why the
// order of entries matters).
func (s *Server) audit(ctx context.Context, q execQuerier, ev auditEvent) {
	s.auditRecord(ctx, q, ev)()
}

// auditRecord inserts the entry through q and returns the function that logs it. For a
// change made in a transaction, pass the transaction and call the returned function only
// after Commit succeeds, so a rolled-back change never ships an "audit" line.
func (s *Server) auditRecord(ctx context.Context, q execQuerier, ev auditEvent) func() {
	emit, err := s.auditInsert(ctx, q, ev)
	if err != nil {
		ctxLogger(ctx).Error("write audit log", "action", ev.action, "err", err)
	}
	return emit
}

// auditInsert is auditRecord for the one entry that must not be lost, site.create: it
// returns the insert error to the caller, who rolls the change back. The returned
// function is always usable and logs the entry.
func (s *Server) auditInsert(ctx context.Context, q execQuerier, ev auditEvent) (func(), error) {
	src, _ := ctx.Value(auditSourceKey{}).(auditSource)
	uid, _ := ctx.Value(userIDKey).(int64)
	details := []byte("{}")
	if len(ev.details) > 0 {
		var err error
		if details, err = json.Marshal(ev.details); err != nil {
			details = []byte("{}")
			ctxLogger(ctx).Error("encode audit details", "action", ev.action, "err", err)
		}
	}
	var (
		id    int64
		email string
	)
	// The email is copied by the INSERT itself, so the entry keeps the address the user had then.
	err := q.QueryRowContext(context.WithoutCancel(ctx),
		`INSERT INTO audit_log (time, actor, actor_email, actor_via, action, target_kind, target, siteid, groupid, details, client_ip)
		 VALUES (?, ?, COALESCE((SELECT email FROM user WHERE userid = ?), ''), ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING auditid, actor_email`,
		time.Now().Unix(), nullableID(uid), uid, src.via, ev.action, ev.targetKind, ev.target,
		nullableID(ev.siteID), nullableID(ev.groupID), string(details), src.ip).Scan(&id, &email)
	return func() {
		ctxLogger(ctx).Info("audit", "audit_id", id, "actor", uid, "actor_email", oneLine(email), "actor_via", oneLine(src.via),
			"action", ev.action, "target_kind", ev.targetKind, "target", oneLine(ev.target),
			"site_id", ev.siteID, "group_id", ev.groupID, "details", oneLine(string(details)), "client_ip", oneLine(src.ip))
	}, err
}

// oneLine drops line breaks from a value that came from a request. The slog handlers
// already quote such values, so this is belt and braces for log shippers that split on
// raw newlines.
func oneLine(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, "\n", ""), "\r", "")
}

// pruneAudit deletes entries older than the retention period; 0 keeps them for ever.
func (s *Server) pruneAudit(ctx context.Context) error {
	if s.cfg.AuditRetention <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -s.cfg.AuditRetention).Unix()
	res, err := s.db.ExecContext(ctx, "DELETE FROM audit_log WHERE time < ?", cutoff)
	if err != nil {
		return fmt.Errorf("prune audit log: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Info("pruned audit log", "deleted", n, "retention_days", s.cfg.AuditRetention)
	}
	return nil
}

// startAuditPruner prunes once now and then every day until Close.
func (s *Server) startAuditPruner() {
	ctx, cancel := context.WithCancel(context.Background())
	s.stopBackground = cancel
	if err := s.pruneAudit(ctx); err != nil {
		slog.Error("prune audit log", "err", err)
	}
	s.background.Go(func() {
		t := time.NewTicker(auditPruneEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.pruneAudit(ctx); err != nil && ctx.Err() == nil {
					slog.Error("prune audit log", "err", err)
				}
			}
		}
	})
}

// auditEntry is one entry as the API returns it.
type auditEntry struct {
	ID         int64           `json:"id"`
	Time       string          `json:"time"`
	Actor      *int64          `json:"actor"` // null for entries without a user
	ActorEmail string          `json:"actor_email"`
	ActorVia   string          `json:"actor_via"`
	Action     string          `json:"action"`
	TargetKind string          `json:"target_kind"`
	Target     string          `json:"target"`
	Details    json.RawMessage `json:"details"`
	ClientIP   string          `json:"client_ip"`
}

// auditPage reads the limit and before query parameters. It writes the 400 itself and
// returns false on a bad value.
func auditPage(w http.ResponseWriter, r *http.Request) (limit int, before int64, ok bool) {
	limit = auditDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a whole number of at least 1")
			return 0, 0, false
		}
		limit = min(n, auditMaxLimit)
	}
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be an entry id")
			return 0, 0, false
		}
		before = n
	}
	return limit, before, true
}

// An audit page query is auditSelect, a WHERE condition, then auditPageTail, all
// constants so no SQL is built at run time. The tail takes the before cursor (twice) and
// the row limit.
const (
	auditSelect = `SELECT auditid, time, actor, actor_email, actor_via, action, target_kind, target, details, client_ip
		 FROM audit_log WHERE `
	auditPageTail = ` AND (? = 0 OR auditid < ?) ORDER BY auditid DESC LIMIT ?`
)

// writeAuditPage answers with the newest entries of query (see auditSelect) with its args
// that are older than the before cursor. next_before is the cursor for the following page,
// or null on the last one.
func (s *Server) writeAuditPage(w http.ResponseWriter, r *http.Request, query string, args ...any) {
	limit, before, ok := auditPage(w, r)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), query, append(args, before, before, limit+1)...)
	if err != nil {
		internalError(w, "query audit log", err)
		return
	}
	defer rows.Close()
	entries := []auditEntry{}
	for rows.Next() {
		var (
			e     auditEntry
			at    int64
			actor sql.NullInt64
			det   string
		)
		if err := rows.Scan(&e.ID, &at, &actor, &e.ActorEmail, &e.ActorVia, &e.Action, &e.TargetKind, &e.Target, &det, &e.ClientIP); err != nil {
			internalError(w, "scan audit entry", err)
			return
		}
		e.Time = formatUnix(at)
		e.Details = json.RawMessage(det)
		if actor.Valid {
			e.Actor = &actor.Int64
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "query audit log", err)
		return
	}
	var next *int64
	if len(entries) > limit {
		entries = entries[:limit]
		next = &entries[limit-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "next_before": next})
}

// handleSiteAudit: GET /v1/auth/sites/{name}/audit. Owner only, like the other manage
// routes. It lists what happened to the site since it was last created: a site deleted
// and recreated under the same name starts with an empty history, so a new owner never
// sees the previous owner's entries. The history of a deleted site is no longer reachable
// through the API.
func (s *Server) handleSiteAudit(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	// A rowid can be reused after its row is deleted, so the site id alone does not tell
	// two sites apart; entries older than this site's create entry belong to a predecessor.
	// createSite writes that entry in the same transaction as the site row, so the boundary
	// exists from the moment the site does and a failed insert means no site. Sites that
	// predate the audit log have no create entry and keep all their entries.
	s.writeAuditPage(w, r, auditSelect+
		`target_kind = 'site' AND target = ? AND siteid = ?
		 AND auditid >= COALESCE((SELECT MAX(auditid) FROM audit_log WHERE action = 'site.create' AND siteid = ? AND target = ?), 0)`+
		auditPageTail,
		site.Name, site.ID, site.ID, site.Name)
}

// handleUserAudit: GET /v1/auth/user/audit lists what the caller did.
func (s *Server) handleUserAudit(w http.ResponseWriter, r *http.Request) {
	s.writeAuditPage(w, r, auditSelect+"actor = ?"+auditPageTail, userID(r))
}
