// Package store persists queries, replies and images in PostgreSQL (schema "queries").
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a query or file does not exist.
var ErrNotFound = errors.New("not found")

// Image is a validated image ready to be stored.
type Image struct {
	MIME string
	Ext  string
	Data []byte
}

// NewQuery is a visitor submission.
type NewQuery struct {
	ID        string
	IPHash    string
	UserAgent string
	Body      string
	Images    []Image
}

// FileRef identifies a stored image.
type FileRef struct {
	ID     string
	MIME   string
	Ext    string
	Size   int
	Inline bool // set by callers that know browser support
}

// Message is one entry in a thread as shown to the visitor.
type Message struct {
	FromOwner bool
	At        time.Time
	Text      string
	Files     []FileRef
}

// Thread is a query with its conversation.
type Thread struct {
	ID        string
	CreatedAt time.Time
	Messages  []Message // first entry is the visitor's own query
}

// PendingQuery is a query that still has to be delivered to Slack.
type PendingQuery struct {
	ID        string
	CreatedAt time.Time
	IPHash    string
	Body      string
	SlackTS   string // empty until the root message is posted
	Attempts  int
}

// UnsentFile is a visitor image not yet uploaded to Slack.
type UnsentFile struct {
	ID       string
	Position int
	MIME     string
	Ext      string
	Data     []byte
}

// ThreadRef links a query to its Slack thread.
type ThreadRef struct {
	QueryID string
	SlackTS string
}

// NewReply is an owner reply to store.
type NewReply struct {
	QueryID string
	SlackTS string
	At      time.Time
	Body    string
	Hidden  bool
	Images  []Image
}

// PendingFollowUp is a visitor follow-up that still has to be delivered to Slack.
type PendingFollowUp struct {
	ID       int64
	QueryID  string
	ThreadTS string
	Body     string
	SlackTS  string // empty until the text is posted
	Attempts int
}

// Store wraps a pgx pool.
type Store struct{ pool *pgxpool.Pool }

// Open connects (retrying while the database starts) and migrates.
func Open(ctx context.Context, host, port, db, user, password string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("host=%s port=%s dbname=%s user=%s sslmode=disable", host, port, db, user))
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Password = password
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour

	var pool *pgxpool.Pool
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("database not reachable: %w", err)
		}
		slog.Warn("waiting for database", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const migrationLockID = 727275 // distinct from the chatbot's lock

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()

	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS queries`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS queries.schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM queries.schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO queries.schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		slog.Info("applied migration", "version", name)
	}
	return nil
}

// CountToday returns queries received since 00:00 UTC.
func (s *Store) CountToday(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM queries.queries
		WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&n)
	return n, err
}

// CreateQuery stores a query and its images atomically.
func (s *Store) CreateQuery(ctx context.Context, q NewQuery) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO queries.queries (id, ip_hash, user_agent, body) VALUES ($1,$2,$3,$4)`,
		q.ID, q.IPHash, q.UserAgent, q.Body); err != nil {
		return err
	}
	for i, im := range q.Images {
		if _, err := tx.Exec(ctx, `INSERT INTO queries.files (query_id, position, mime, ext, size, data) VALUES ($1,$2,$3,$4,$5,$6)`,
			q.ID, i, im.MIME, im.Ext, len(im.Data), im.Data); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ClaimPending leases the next query that needs delivery to Slack. The lease
// keeps other workers away while this one runs; MarkFailed/MarkDone replace it.
func (s *Store) ClaimPending(ctx context.Context, lease time.Duration) (*PendingQuery, error) {
	var p PendingQuery
	var ts *string
	err := s.pool.QueryRow(ctx, `
		UPDATE queries.queries SET next_attempt_at = now() + make_interval(secs => $1)
		WHERE id = (
			SELECT id FROM queries.queries
			WHERE NOT slack_done AND next_attempt_at <= now()
			ORDER BY next_attempt_at, created_at
			FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, created_at, ip_hash, body, slack_ts, slack_attempts`, lease.Seconds()).
		Scan(&p.ID, &p.CreatedAt, &p.IPHash, &p.Body, &ts, &p.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ts != nil {
		p.SlackTS = *ts
	}
	return &p, nil
}

// SetRootPosted records the Slack thread root immediately after posting it.
func (s *Store) SetRootPosted(ctx context.Context, id, ts string) error {
	_, err := s.pool.Exec(ctx, `UPDATE queries.queries SET slack_ts = $2 WHERE id = $1`, id, ts)
	return err
}

// UnsentFiles lists visitor images still to upload.
func (s *Store) UnsentFiles(ctx context.Context, id string) ([]UnsentFile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, position, mime, ext, data FROM queries.files
		WHERE query_id = $1 AND reply_id IS NULL AND NOT slack_sent ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnsentFile
	for rows.Next() {
		var f UnsentFile
		if err := rows.Scan(&f.ID, &f.Position, &f.MIME, &f.Ext, &f.Data); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkFileSent flags an image as uploaded to Slack.
func (s *Store) MarkFileSent(ctx context.Context, fileID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE queries.files SET slack_sent = true WHERE id = $1`, fileID)
	return err
}

// MarkDone marks a query as fully delivered.
func (s *Store) MarkDone(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE queries.queries SET slack_done = true, last_error = '' WHERE id = $1`, id)
	return err
}

// MarkFailed schedules a retry.
func (s *Store) MarkFailed(ctx context.Context, id, msg string, retryIn time.Duration) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE queries.queries SET slack_attempts = slack_attempts + 1, last_error = $2,
			next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1`, id, msg, retryIn.Seconds())
	return err
}

// RecentThreads returns delivered threads created since the given time.
func (s *Store) RecentThreads(ctx context.Context, since time.Time) ([]ThreadRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, slack_ts FROM queries.queries
		WHERE slack_ts IS NOT NULL AND created_at >= $1 ORDER BY created_at DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThreadRef
	for rows.Next() {
		var t ThreadRef
		if err := rows.Scan(&t.QueryID, &t.SlackTS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryBySlackTS finds the query for a Slack thread root.
func (s *Store) QueryBySlackTS(ctx context.Context, ts string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM queries.queries WHERE slack_ts = $1`, ts).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// KnownReplies returns the Slack timestamps of replies already processed.
func (s *Store) KnownReplies(ctx context.Context, queryID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT slack_ts FROM queries.replies WHERE query_id = $1`, queryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		out[ts] = true
	}
	return out, rows.Err()
}

// AddReply stores an owner reply. It returns false if that Slack message was
// already stored (events and the periodic sync can overlap).
func (s *Store) AddReply(ctx context.Context, r NewReply) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO queries.replies (query_id, created_at, body, slack_ts, hidden) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (slack_ts) DO NOTHING RETURNING id`,
		r.QueryID, r.At, r.Body, r.SlackTS, r.Hidden).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for i, im := range r.Images {
		if _, err := tx.Exec(ctx, `
			INSERT INTO queries.files (query_id, reply_id, position, mime, ext, size, data, slack_sent)
			VALUES ($1,$2,$3,$4,$5,$6,$7,true)`, r.QueryID, id, i, im.MIME, im.Ext, len(im.Data), im.Data); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// GetThread loads a query and all visible replies with image references.
func (s *Store) GetThread(ctx context.Context, id string) (*Thread, error) {
	t := &Thread{ID: id}
	var body string
	err := s.pool.QueryRow(ctx, `SELECT created_at, body FROM queries.queries WHERE id = $1`, id).Scan(&t.CreatedAt, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	first := Message{At: t.CreatedAt, Text: body}
	replies := map[int64]*Message{}
	var order []int64

	rows, err := s.pool.Query(ctx, `SELECT id, created_at, body, sender = 'owner' FROM queries.replies WHERE query_id = $1 AND NOT hidden ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rid int64
		m := &Message{}
		if err := rows.Scan(&rid, &m.At, &m.Text, &m.FromOwner); err != nil {
			rows.Close()
			return nil, err
		}
		replies[rid] = m
		order = append(order, rid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frows, err := s.pool.Query(ctx, `
		SELECT id::text, reply_id, mime, ext, size FROM queries.files WHERE query_id = $1 ORDER BY position, created_at`, id)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var f FileRef
		var rid *int64
		if err := frows.Scan(&f.ID, &rid, &f.MIME, &f.Ext, &f.Size); err != nil {
			return nil, err
		}
		if rid == nil {
			first.Files = append(first.Files, f)
		} else if m, ok := replies[*rid]; ok {
			m.Files = append(m.Files, f)
		}
	}
	if err := frows.Err(); err != nil {
		return nil, err
	}
	t.Messages = append(t.Messages, first)
	for _, rid := range order {
		t.Messages = append(t.Messages, *replies[rid])
	}
	return t, nil
}

// GetFile returns an image of a query.
func (s *Store) GetFile(ctx context.Context, queryID, fileID string) (mime string, data []byte, err error) {
	err = s.pool.QueryRow(ctx, `SELECT mime, data FROM queries.files WHERE id::text = $1 AND query_id = $2`,
		fileID, queryID).Scan(&mime, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return mime, data, err
}

// ErrFollowUpLimit means a thread has reached its follow-up quota.
var ErrFollowUpLimit = errors.New("follow-up limit reached")

// AddFollowUp stores a visitor's follow-up for delivery to Slack. At most
// perThread follow-ups are allowed overall and perWindow within the last 10 minutes.
func (s *Store) AddFollowUp(ctx context.Context, queryID, ipHash, body string, images []Image, perThread, perWindow int) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Serialise follow-ups of one query so the quota cannot be raced.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM queries.queries WHERE id = $1 FOR UPDATE`, queryID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var total, recent int
	if err := tx.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE created_at > now() - interval '10 minutes')
		FROM queries.replies WHERE query_id = $1 AND sender = 'visitor'`, queryID).Scan(&total, &recent); err != nil {
		return 0, err
	}
	if total >= perThread || recent >= perWindow {
		return 0, ErrFollowUpLimit
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO queries.replies (query_id, body, sender, ip_hash, slack_done)
		VALUES ($1,$2,'visitor',$3,false) RETURNING id`, queryID, body, ipHash).Scan(&id); err != nil {
		return 0, err
	}
	for i, im := range images {
		if _, err := tx.Exec(ctx, `
			INSERT INTO queries.files (query_id, reply_id, position, mime, ext, size, data)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, queryID, id, i, im.MIME, im.Ext, len(im.Data), im.Data); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit(ctx)
}

// CountFollowUpsToday returns visitor follow-ups received since 00:00 UTC.
func (s *Store) CountFollowUpsToday(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM queries.replies WHERE sender = 'visitor'
		AND created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&n)
	return n, err
}

// ClaimFollowUp leases the next follow-up whose Slack thread already exists.
func (s *Store) ClaimFollowUp(ctx context.Context, lease time.Duration) (*PendingFollowUp, error) {
	var p PendingFollowUp
	var ts *string
	err := s.pool.QueryRow(ctx, `
		UPDATE queries.replies SET next_attempt_at = now() + make_interval(secs => $1)
		WHERE id = (
			SELECT r.id FROM queries.replies r JOIN queries.queries q ON q.id = r.query_id
			WHERE NOT r.slack_done AND r.sender = 'visitor' AND q.slack_ts IS NOT NULL AND r.next_attempt_at <= now()
			ORDER BY r.next_attempt_at, r.id
			FOR UPDATE OF r SKIP LOCKED LIMIT 1)
		RETURNING id, query_id, (SELECT slack_ts FROM queries.queries WHERE id = query_id), body, slack_ts, slack_attempts`,
		lease.Seconds()).Scan(&p.ID, &p.QueryID, &p.ThreadTS, &p.Body, &ts, &p.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ts != nil {
		p.SlackTS = *ts
	}
	return &p, nil
}

// SetFollowUpPosted records the Slack message of a follow-up right after posting it.
func (s *Store) SetFollowUpPosted(ctx context.Context, replyID int64, ts string) error {
	_, err := s.pool.Exec(ctx, `UPDATE queries.replies SET slack_ts = $2 WHERE id = $1`, replyID, ts)
	return err
}

// UnsentReplyFiles lists a follow-up's images still to upload.
func (s *Store) UnsentReplyFiles(ctx context.Context, replyID int64) ([]UnsentFile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, position, mime, ext, data FROM queries.files
		WHERE reply_id = $1 AND NOT slack_sent ORDER BY position`, replyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnsentFile
	for rows.Next() {
		var f UnsentFile
		if err := rows.Scan(&f.ID, &f.Position, &f.MIME, &f.Ext, &f.Data); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MarkFollowUpDone marks a follow-up as fully delivered.
func (s *Store) MarkFollowUpDone(ctx context.Context, replyID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE queries.replies SET slack_done = true, last_error = '' WHERE id = $1`, replyID)
	return err
}

// MarkFollowUpFailed schedules a retry.
func (s *Store) MarkFollowUpFailed(ctx context.Context, replyID int64, msg string, retryIn time.Duration) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE queries.replies SET slack_attempts = slack_attempts + 1, last_error = $2,
			next_attempt_at = now() + make_interval(secs => $3) WHERE id = $1`, replyID, msg, retryIn.Seconds())
	return err
}
