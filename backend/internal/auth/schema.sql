-- Accounts, sessions and board access. Applied at startup; idempotent.

CREATE TABLE IF NOT EXISTS users (
	id         text        PRIMARY KEY,
	name       text        NOT NULL,
	avatar_url text        NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);

-- One row per provider identity; several may point at the same user.
CREATE TABLE IF NOT EXISTS oauth_accounts (
	provider   text        NOT NULL,
	subject    text        NOT NULL,
	user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	created_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (provider, subject)
);

-- token_hash is SHA-256 of the cookie value; the token itself is never stored.
CREATE TABLE IF NOT EXISTS sessions (
	token_hash bytea       PRIMARY KEY,
	user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions (expires_at);

-- The owner is kept apart from other members so that claiming an unowned
-- board is a single INSERT ... ON CONFLICT, which cannot race.
CREATE TABLE IF NOT EXISTS board_owners (
	board    text        PRIMARY KEY,
	user_id  text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	claimed_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS board_members (
	board   text NOT NULL,
	user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	role    text NOT NULL CHECK (role IN ('editor', 'viewer')),
	PRIMARY KEY (board, user_id)
);

CREATE TABLE IF NOT EXISTS board_invites (
	token_hash bytea       PRIMARY KEY,
	board      text        NOT NULL,
	role       text        NOT NULL CHECK (role IN ('editor', 'viewer')),
	created_by text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	expires_at timestamptz NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
