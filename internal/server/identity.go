package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/gira0/open-pages/internal/names"
)

// Code shared by the OIDC and SAML sign-in flows. Both store their identities in the same
// columns (user.oidc_issuer, user.oidc_subject) and mark provider-managed group memberships
// with user_group.oidc = 1; the column names predate SAML and are kept (see docs/schema.md).

var (
	errNoEmail     = errors.New("the identity has no usable email address")
	errEmailDomain = errors.New("email domain is not allowed")
	errEmailTaken  = errors.New("an account with this email already exists")
)

// checkEmail requires a bare address and, when domain is set (lower case), that the address
// belongs to it.
func checkEmail(email, domain string) error {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > maxEmailLen {
		return errNoEmail
	}
	if domain != "" && strings.ToLower(email[strings.LastIndex(email, "@")+1:]) != domain {
		return errEmailDomain
	}
	return nil
}

// federatedIdentity is what a verified identity provider assertion (an OIDC ID token or a SAML
// assertion) says about the user. issuer is the provider's identifier; subject is stable
// per user within it.
type federatedIdentity struct {
	issuer, subject, email string
	groups                 []string
}

// federatedUser returns the local user for an identity, creating it on first login. Users match
// on issuer plus subject only. An email that already belongs to a different account, local
// or from another identity, is refused: it is never taken over or linked automatically.
func (s *Server) federatedUser(ctx context.Context, id federatedIdentity) (int64, error) {
	lookup := func() (int64, error) {
		var uid int64
		err := s.db.QueryRowContext(ctx,
			"SELECT userid FROM user WHERE oidc_issuer = ? AND oidc_subject = ?", id.issuer, id.subject).Scan(&uid)
		return uid, err
	}
	uid, err := lookup()
	if err == nil {
		return uid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup federated user: %w", err)
	}
	// The empty password hash never matches, so the account can't use local login. The
	// case-insensitive email check is part of the INSERT, so two concurrent first logins
	// with case variants of one email can't both succeed.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user (email, password, oidc_issuer, oidc_subject)
		 SELECT ?, '', ?, ? WHERE NOT EXISTS (SELECT 1 FROM user WHERE email = ? COLLATE NOCASE)
		 ON CONFLICT DO NOTHING`,
		id.email, id.issuer, id.subject, id.email)
	if err != nil {
		return 0, fmt.Errorf("insert federated user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A concurrent first login of the same identity may have won; otherwise the email was.
		if uid, err := lookup(); err == nil {
			return uid, nil
		}
		return 0, errEmailTaken
	}
	uid, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("federated user id: %w", err)
	}
	return uid, nil
}

// syncFederatedGroups makes the user's provider-managed memberships match the group names in
// the identity provider's assertion. Only existing groups are mapped (matched like group names everywhere: ignoring
// case); unknown names are ignored. Memberships added by hand (oidc = 0) are never removed.
func (s *Server) syncFederatedGroups(ctx context.Context, uid int64, groupNames []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var want []int64
	for _, name := range groupNames {
		name, ok := validGroupName(name)
		if !ok {
			continue
		}
		var gid int64
		err := tx.QueryRowContext(ctx, "SELECT groupid FROM groups WHERE name_key = ?", names.GroupKey(name)).Scan(&gid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find group: %w", err)
		}
		want = append(want, gid)
	}
	rows, err := tx.QueryContext(ctx, "SELECT gid FROM user_group WHERE uid = ? AND oidc = 1", uid)
	if err != nil {
		return fmt.Errorf("list memberships: %w", err)
	}
	var have []int64
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan membership: %w", err)
		}
		have = append(have, gid)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, gid := range have {
		if slices.Contains(want, gid) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_group WHERE uid = ? AND gid = ? AND oidc = 1", uid, gid); err != nil {
			return fmt.Errorf("remove membership: %w", err)
		}
	}
	for _, gid := range want {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user_group (uid, gid, oidc) VALUES (?, ?, 1) ON CONFLICT DO NOTHING", uid, gid); err != nil {
			return fmt.Errorf("add membership: %w", err)
		}
	}
	return tx.Commit()
}
