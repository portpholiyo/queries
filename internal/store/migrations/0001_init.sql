-- Visitor queries, the owner's replies (from Slack) and image attachments.
CREATE TABLE queries.queries (
    id              TEXT PRIMARY KEY,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip_hash         TEXT NOT NULL,
    user_agent      TEXT NOT NULL DEFAULT '',
    body            TEXT NOT NULL,
    slack_ts        TEXT UNIQUE,                       -- thread root once posted to Slack
    slack_done      BOOLEAN NOT NULL DEFAULT false,    -- root and all images delivered
    slack_attempts  INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX queries_pending_idx ON queries.queries (next_attempt_at) WHERE NOT slack_done;
CREATE INDEX queries_created_idx ON queries.queries (created_at DESC);

CREATE TABLE queries.replies (
    id         BIGSERIAL PRIMARY KEY,
    query_id   TEXT NOT NULL REFERENCES queries.queries (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    body       TEXT NOT NULL,
    slack_ts   TEXT NOT NULL UNIQUE,                   -- makes processing idempotent
    hidden     BOOLEAN NOT NULL DEFAULT false         -- seen but nothing to show (e.g. unsupported file only)
);
CREATE INDEX replies_query_idx ON queries.replies (query_id, id);

CREATE TABLE queries.files (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    query_id   TEXT NOT NULL REFERENCES queries.queries (id) ON DELETE CASCADE,
    reply_id   BIGINT REFERENCES queries.replies (id) ON DELETE CASCADE, -- NULL = sent by the visitor
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    position   INTEGER NOT NULL DEFAULT 0,
    mime       TEXT NOT NULL,
    ext        TEXT NOT NULL,
    size       INTEGER NOT NULL,
    data       BYTEA NOT NULL,
    slack_sent BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX files_query_idx ON queries.files (query_id);
