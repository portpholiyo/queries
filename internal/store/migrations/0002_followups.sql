-- Visitors can reply again. Their follow-ups live in the same table as the owner's
-- replies and are delivered to the Slack thread by the same outbox pattern as queries.
ALTER TABLE queries.replies
    ADD COLUMN sender          TEXT NOT NULL DEFAULT 'owner' CHECK (sender IN ('owner', 'visitor')),
    ADD COLUMN ip_hash         TEXT NOT NULL DEFAULT '',
    ADD COLUMN slack_done      BOOLEAN NOT NULL DEFAULT true,   -- false while a visitor follow-up awaits delivery
    ADD COLUMN slack_attempts  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN last_error      TEXT NOT NULL DEFAULT '';
ALTER TABLE queries.replies ALTER COLUMN slack_ts DROP NOT NULL;  -- set once the follow-up is posted
CREATE INDEX replies_pending_idx ON queries.replies (next_attempt_at) WHERE NOT slack_done;
