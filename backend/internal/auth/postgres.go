package auth

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/highlvmami/lumoraboard/backend/internal/proto"
)

//go:embed schema.sql
var schema string

// Postgres is the production Store.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres applies the schema on pool and returns the store. The
// caller owns the pool.
func NewPostgres(ctx context.Context, pool *pgxpool.Pool) (*Postgres, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("auth: apply schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) UpsertUser(ctx context.Context, provider string, prof Profile) (User, error) {
	var u User
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT user_id FROM oauth_accounts WHERE provider = $1 AND subject = $2`,
			provider, prof.Subject).Scan(&u.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			u.ID = newID()
			if _, err := tx.Exec(ctx, `INSERT INTO users (id, name, avatar_url) VALUES ($1, $2, $3)`,
				u.ID, prof.Name, prof.Avatar); err != nil {
				return err
			}
			// Two first sign-ins racing for one identity: the loser's
			// insert conflicts and its transaction rolls back; the
			// caller retries and finds the winner's row.
			_, err = tx.Exec(ctx, `INSERT INTO oauth_accounts (provider, subject, user_id) VALUES ($1, $2, $3)`,
				provider, prof.Subject, u.ID)
			return err
		case err != nil:
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET name = $2, avatar_url = $3 WHERE id = $1`, u.ID, prof.Name, prof.Avatar)
		return err
	})
	if err != nil {
		return User{}, fmt.Errorf("auth: upsert user: %w", err)
	}
	u.Name, u.Avatar = prof.Name, prof.Avatar
	return u, nil
}

func (p *Postgres) CreateSession(ctx context.Context, hash []byte, userID string, expires time.Time) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`, hash, userID, expires)
	if err != nil {
		return fmt.Errorf("auth: create session: %w", err)
	}
	return nil
}

func (p *Postgres) SessionUser(ctx context.Context, hash []byte, now time.Time) (User, error) {
	var u User
	err := p.pool.QueryRow(ctx, `
		SELECT u.id, u.name, u.avatar_url FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hash, now).Scan(&u.ID, &u.Name, &u.Avatar)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: session: %w", err)
	}
	return u, nil
}

func (p *Postgres) DeleteSession(ctx context.Context, hash []byte) error {
	if _, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

func (p *Postgres) ClaimBoard(ctx context.Context, board, userID string) (proto.Role, error) {
	var role string
	// The CTE claims ownership if nobody holds it, then the outer query
	// reads the user's role whether or not the claim won.
	err := p.pool.QueryRow(ctx, `
		WITH claim AS (
			INSERT INTO board_owners (board, user_id) VALUES ($1, $2)
			ON CONFLICT (board) DO NOTHING
			RETURNING user_id
		)
		SELECT CASE
			WHEN EXISTS (SELECT 1 FROM claim) THEN 'owner'
			WHEN EXISTS (SELECT 1 FROM board_owners WHERE board = $1 AND user_id = $2) THEN 'owner'
			ELSE COALESCE((SELECT role FROM board_members WHERE board = $1 AND user_id = $2), '')
		END`, board, userID).Scan(&role)
	if err != nil {
		return "", fmt.Errorf("auth: claim board: %w", err)
	}
	return proto.Role(role), nil
}

func (p *Postgres) CreateInvite(ctx context.Context, hash []byte, board string, role proto.Role, by string, expires time.Time) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO board_invites (token_hash, board, role, created_by, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash, board, string(role), by, expires)
	if err != nil {
		return fmt.Errorf("auth: create invite: %w", err)
	}
	return nil
}

func (p *Postgres) AcceptInvite(ctx context.Context, hash []byte, userID string, now time.Time) (string, proto.Role, error) {
	var (
		board string
		role  string
	)
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT board, role FROM board_invites WHERE token_hash = $1 AND expires_at > $2`, hash, now).Scan(&board, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var isOwner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM board_owners WHERE board = $1 AND user_id = $2)`, board, userID).Scan(&isOwner); err != nil {
			return err
		}
		if isOwner {
			role = string(proto.RoleOwner)
			return nil
		}
		// Keep the stronger of the existing and the invited role.
		return tx.QueryRow(ctx, `
			INSERT INTO board_members (board, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT (board, user_id) DO UPDATE
			SET role = CASE WHEN board_members.role = 'editor' THEN 'editor' ELSE excluded.role END
			RETURNING role`, board, userID, role).Scan(&role)
	})
	if errors.Is(err, ErrNotFound) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("auth: accept invite: %w", err)
	}
	return board, proto.Role(role), nil
}
