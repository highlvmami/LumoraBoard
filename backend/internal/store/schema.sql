-- Applied at startup; every statement is idempotent.

CREATE TABLE IF NOT EXISTS boards (
	name       text        PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);

-- Only the latest snapshot per board is kept; older ones are replaced.
CREATE TABLE IF NOT EXISTS board_snapshots (
	board      text        PRIMARY KEY REFERENCES boards (name) ON DELETE CASCADE,
	seq        bigint      NOT NULL,
	objects    jsonb       NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- Ops accepted after the snapshot. Compaction deletes the ones a newer
-- snapshot covers.
CREATE TABLE IF NOT EXISTS board_ops (
	board        text        NOT NULL REFERENCES boards (name) ON DELETE CASCADE,
	seq          bigint      NOT NULL,
	sender       text        NOT NULL,
	client_op_id text        NOT NULL,
	op           jsonb       NOT NULL,
	created_at   timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (board, seq)
);

-- Chat is kept in full; ids are per board, assigned by the room.
CREATE TABLE IF NOT EXISTS chat_messages (
	board      text        NOT NULL,
	id         bigint      NOT NULL,
	sender     text        NOT NULL,
	user_id    text        NOT NULL DEFAULT '',
	name       text        NOT NULL,
	avatar_url text        NOT NULL DEFAULT '',
	text       text        NOT NULL,
	ref        text        NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL,
	PRIMARY KEY (board, id)
);
