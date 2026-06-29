package main

import (
	"context"
	"database/sql"
	"time"
)

type AsteriskRoute struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	WANumber  string `json:"waNumber"`
	SIPTarget string `json:"sipTarget"`
	SIPFrom   string `json:"sipFrom"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

type asteriskRouteStore struct{ db *sql.DB }

func newAsteriskRouteStore(ctx context.Context, db *sql.DB) (*asteriskRouteStore, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS asterisk_routes (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		wa_number  TEXT NOT NULL,
		sip_target TEXT NOT NULL,
		sip_from   TEXT NOT NULL DEFAULT '',
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	return &asteriskRouteStore{db: db}, nil
}

func (s *asteriskRouteStore) list(ctx context.Context) ([]AsteriskRoute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, wa_number, sip_target, sip_from, enabled, created_at, updated_at
		FROM asterisk_routes ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AsteriskRoute
	for rows.Next() {
		r, err := scanAsteriskRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *asteriskRouteStore) findForSession(ctx context.Context, sessionID, waNumber string) (AsteriskRoute, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, wa_number, sip_target, sip_from, enabled, created_at, updated_at
		FROM asterisk_routes
		WHERE enabled = 1 AND (session_id = ? OR wa_number = ?)
		ORDER BY CASE WHEN session_id = ? THEN 0 ELSE 1 END, rowid
		LIMIT 1`, sessionID, waNumber, sessionID)
	r, err := scanAsteriskRoute(row)
	if err == sql.ErrNoRows {
		return AsteriskRoute{}, false, nil
	}
	if err != nil {
		return AsteriskRoute{}, false, err
	}
	return r, true, nil
}

func (s *asteriskRouteStore) insert(ctx context.Context, r AsteriskRoute) (AsteriskRoute, error) {
	now := time.Now().UnixMilli()
	if r.ID == "" {
		r.ID = newSessionID()
	}
	r.CreatedAt = now
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO asterisk_routes
		(id, session_id, wa_number, sip_target, sip_from, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SessionID, r.WANumber, r.SIPTarget, r.SIPFrom, boolInt(r.Enabled), r.CreatedAt, r.UpdatedAt)
	return r, err
}

func (s *asteriskRouteStore) update(ctx context.Context, id string, r AsteriskRoute) (AsteriskRoute, bool, error) {
	r.ID = id
	r.UpdatedAt = time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE asterisk_routes
		SET session_id = ?, wa_number = ?, sip_target = ?, sip_from = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		r.SessionID, r.WANumber, r.SIPTarget, r.SIPFrom, boolInt(r.Enabled), r.UpdatedAt, id)
	if err != nil {
		return AsteriskRoute{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return AsteriskRoute{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, wa_number, sip_target, sip_from, enabled, created_at, updated_at
		FROM asterisk_routes WHERE id = ?`, id)
	out, err := scanAsteriskRoute(row)
	return out, err == nil, err
}

func (s *asteriskRouteStore) delete(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM asterisk_routes WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type routeScanner interface {
	Scan(dest ...any) error
}

func scanAsteriskRoute(row routeScanner) (AsteriskRoute, error) {
	var r AsteriskRoute
	var enabled int
	err := row.Scan(&r.ID, &r.SessionID, &r.WANumber, &r.SIPTarget, &r.SIPFrom, &enabled, &r.CreatedAt, &r.UpdatedAt)
	r.Enabled = enabled != 0
	return r, err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
