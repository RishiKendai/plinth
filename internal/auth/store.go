package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ db *pgxpool.Pool }

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

type user struct {
	ID           string
	Email        string
	DisplayName  string
	PasswordHash string
	LockedUntil  *time.Time
	DeletionAt   *time.Time
	CreatedAt    time.Time
}

const userCols = `id, email, display_name, password_hash, locked_until, deletion_requested_at, created_at`

func scanUser(row pgx.Row) (*user, error) {
	var u user
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.LockedUntil, &u.DeletionAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &u, err
}

var errEmailTaken0 = errors.New("email taken")

func (s *Store) createUser(ctx context.Context, email, hash, name string) (*user, error) {
	var u *user
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		u, err = scanUser(tx.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, display_name) VALUES ($1, $2, $3) RETURNING `+userCols, email, hash, name))
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO user_settings (user_id) VALUES ($1)`, u.ID); err != nil {
			return err
		}
		return nil
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, errEmailTaken0
	}
	return u, err
}

func (s *Store) userByEmail(ctx context.Context, email string) (*user, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE email = $1`, email))
}

func (s *Store) userByID(ctx context.Context, id string) (*user, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

func (s *Store) recordFailedLogin(ctx context.Context, id string, max int, lock time.Duration, now time.Time) error {
	_, err := s.db.Exec(ctx, `
		UPDATE users SET
		  locked_until  = CASE WHEN failed_logins + 1 >= $2 THEN $3::timestamptz ELSE locked_until END,
		  failed_logins = CASE WHEN failed_logins + 1 >= $2 THEN 0 ELSE failed_logins + 1 END
		WHERE id = $1`, id, max, now.Add(lock))
	return err
}

func (s *Store) clearFailedLogins(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = $1 AND (failed_logins <> 0 OR locked_until IS NOT NULL)`, id)
	return err
}

type sessionRow struct {
	ID, UserID string
	DeviceName string
	Platform   string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

type newSession struct {
	UserID, Device, Platform           string
	AccessHash, RefreshHash            []byte
	AccessExpires, RefreshExpires, Now time.Time
}

func (s *Store) createSession(ctx context.Context, n newSession) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `
		INSERT INTO sessions (user_id, access_hash, refresh_hash, access_expires_at, refresh_expires_at, device_name, platform, created_at, last_used_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8) RETURNING id`,
		n.UserID, n.AccessHash, n.RefreshHash, n.AccessExpires, n.RefreshExpires, n.Device, n.Platform, n.Now).Scan(&id)
	return id, err
}

// authenticate resolves a live session from an access-token hash.
func (s *Store) authenticate(ctx context.Context, accessHash []byte, now time.Time) (Principal, error) {
	var p Principal
	var last time.Time
	err := s.db.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.last_used_at FROM sessions s
		WHERE s.access_hash = $1 AND s.revoked_at IS NULL AND s.access_expires_at > $2`, accessHash, now).Scan(&p.SessionID, &p.UserID, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrUnauthenticated
	}
	if err == nil && now.Sub(last) > time.Minute {
		_, _ = s.db.Exec(ctx, `UPDATE sessions SET last_used_at = $2 WHERE id = $1`, p.SessionID, now)
	}
	return p, err
}

var ErrUnauthenticated = errors.New("unauthenticated")
var errRefreshReuse = errors.New("refresh token reuse")

type rotation struct {
	OldRefreshHash, NewAccessHash, NewRefreshHash []byte
	AccessExpires, RefreshExpires, Now            time.Time
}

// rotate swaps both tokens for a new pair. Presenting an already-rotated refresh token revokes the whole session.
func (s *Store) rotate(ctx context.Context, r rotation) (userID, sessionID string, err error) {
	reuse := false
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		qerr := tx.QueryRow(ctx, `SELECT id, user_id FROM sessions
			WHERE refresh_hash = $1 AND revoked_at IS NULL AND refresh_expires_at > $2 FOR UPDATE`, r.OldRefreshHash, r.Now).Scan(&sessionID, &userID)
		if errors.Is(qerr, pgx.ErrNoRows) {
			var id, uid string
			perr := tx.QueryRow(ctx, `SELECT id, user_id FROM sessions WHERE prev_refresh_hash = $1 AND revoked_at IS NULL`, r.OldRefreshHash).Scan(&id, &uid)
			if perr == nil {
				if _, e := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1`, id, r.Now); e != nil {
					return e
				}
				reuse = true
				return nil // commit the revocation
			}
			if errors.Is(perr, pgx.ErrNoRows) {
				return ErrUnauthenticated
			}
			return perr
		}
		if qerr != nil {
			return qerr
		}
		_, e := tx.Exec(ctx, `UPDATE sessions SET access_hash=$2, refresh_hash=$3, prev_refresh_hash=$4,
			access_expires_at=$5, refresh_expires_at=$6, last_used_at=$7 WHERE id=$1`,
			sessionID, r.NewAccessHash, r.NewRefreshHash, r.OldRefreshHash, r.AccessExpires, r.RefreshExpires, r.Now)
		return e
	})
	if err == nil && reuse {
		return "", "", errRefreshReuse
	}
	return userID, sessionID, err
}

func (s *Store) revokeSession(ctx context.Context, userID, sessionID string, now time.Time) (bool, error) {
	if !validUUID(sessionID) {
		return false, nil
	}
	tag, err := s.db.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sessionID, userID, now)
	return tag.RowsAffected() > 0, err
}

func (s *Store) revokeOthers(ctx context.Context, userID, keep string, now time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keep, now)
	return tag.RowsAffected(), err
}

func (s *Store) listSessions(ctx context.Context, userID string, now time.Time) ([]sessionRow, error) {
	rows, err := s.db.Query(ctx, `SELECT id, device_name, platform, created_at, last_used_at FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND refresh_expires_at > $2 ORDER BY last_used_at DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		r := sessionRow{UserID: userID}
		if err := rows.Scan(&r.ID, &r.DeviceName, &r.Platform, &r.CreatedAt, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// setPassword stores a new hash, clears lockout and revokes all sessions except keepSession (may be empty).
func setPassword(ctx context.Context, tx pgx.Tx, userID, hash, keepSession string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash=$2, failed_logins=0, locked_until=NULL, updated_at=$3 WHERE id=$1`, userID, hash, now); err != nil {
		return err
	}
	if keepSession == "" {
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, now)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=$3 WHERE user_id=$1 AND id <> $2 AND revoked_at IS NULL`, userID, keepSession, now)
	return err
}

func (s *Store) changePassword(ctx context.Context, userID, newHash, keepSession string, now time.Time) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := setPassword(ctx, tx, userID, newHash, keepSession, now); err != nil {
			return err
		}
		return nil
	})
}

// Purge removes expired sessions. Called by the worker.
func (s *Store) Purge(ctx context.Context, now time.Time) (sessions int64, err error) {
	ss, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE refresh_expires_at < $1 OR revoked_at < $2`, now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour))
	return ss.RowsAffected(), err
}
