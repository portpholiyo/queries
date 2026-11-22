// Package worker moves data between the database and Slack:
//   - Deliver: posts new queries (text, then images in the thread) with retries.
//   - Sync: pulls the owner's thread replies back into the database.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/portpholiyo/queries/internal/images"
	"github.com/portpholiyo/queries/internal/slackx"
	"github.com/portpholiyo/queries/internal/store"
)

// Store is the persistence used by the worker (implemented by *store.Store).
type Store interface {
	ClaimPending(ctx context.Context, lease time.Duration) (*store.PendingQuery, error)
	SetRootPosted(ctx context.Context, id, ts string) error
	UnsentFiles(ctx context.Context, id string) ([]store.UnsentFile, error)
	MarkFileSent(ctx context.Context, fileID string) error
	MarkDone(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id, msg string, retryIn time.Duration) error
	ClaimFollowUp(ctx context.Context, lease time.Duration) (*store.PendingFollowUp, error)
	SetFollowUpPosted(ctx context.Context, replyID int64, ts string) error
	UnsentReplyFiles(ctx context.Context, replyID int64) ([]store.UnsentFile, error)
	MarkFollowUpDone(ctx context.Context, replyID int64) error
	MarkFollowUpFailed(ctx context.Context, replyID int64, msg string, retryIn time.Duration) error
	RecentThreads(ctx context.Context, since time.Time) ([]store.ThreadRef, error)
	QueryBySlackTS(ctx context.Context, ts string) (string, error)
	KnownReplies(ctx context.Context, queryID string) (map[string]bool, error)
	AddReply(ctx context.Context, r store.NewReply) (bool, error)
}

// Options configures a Worker.
type Options struct {
	Store         Store
	Slack         slackx.Client
	OwnerID       string
	MaxImages     int
	MaxImageBytes int
	// ReconcileEvery is how often recent threads are re-read from Slack, which
	// catches replies that arrived while the service was down.
	ReconcileEvery time.Duration
	ReconcileAge   time.Duration
}

// Worker runs the delivery and sync loops.
type Worker struct {
	o      Options
	wake   chan struct{}
	syncQ  chan string
	mu     sync.Mutex
	queued map[string]bool
}

// New builds a worker.
func New(o Options) *Worker {
	if o.ReconcileEvery == 0 {
		o.ReconcileEvery = 5 * time.Minute
	}
	if o.ReconcileAge == 0 {
		o.ReconcileAge = 7 * 24 * time.Hour
	}
	return &Worker{o: o, wake: make(chan struct{}, 1), syncQ: make(chan string, 256), queued: map[string]bool{}}
}

// Notify wakes the delivery loop (a new query arrived).
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// EnqueueSync asks for a thread to be re-read (a Slack event arrived).
func (w *Worker) EnqueueSync(threadTS string) {
	w.mu.Lock()
	if w.queued[threadTS] {
		w.mu.Unlock()
		return
	}
	w.queued[threadTS] = true
	w.mu.Unlock()
	select {
	case w.syncQ <- threadTS:
	default:
		w.mu.Lock()
		delete(w.queued, threadTS)
		w.mu.Unlock() // the periodic reconcile will pick it up
	}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.deliverLoop(ctx) }()
	go func() { defer wg.Done(); w.syncLoop(ctx) }()
	wg.Wait()
}

// ---------- delivery ----------

func (w *Worker) deliverLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			p, err := w.o.Store.ClaimPending(ctx, 2*time.Minute)
			if err != nil {
				slog.Error("claim pending failed", "err", err)
				break
			}
			if p == nil {
				break
			}
			w.deliver(ctx, p)
		}
		for ctx.Err() == nil {
			f, err := w.o.Store.ClaimFollowUp(ctx, 2*time.Minute)
			if err != nil {
				slog.Error("claim follow-up failed", "err", err)
				break
			}
			if f == nil {
				break
			}
			w.deliverFollowUp(ctx, f)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-t.C:
		}
	}
}

// RootText builds the Slack message for a query.
func RootText(p *store.PendingQuery, imageCount int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, ":incoming_envelope: *New query* `%s`\n", p.ID)
	fmt.Fprintf(&sb, "_%s UTC · visitor %s", p.CreatedAt.UTC().Format("2 Jan 2006 15:04"), safeHash(p.IPHash))
	if imageCount > 0 {
		fmt.Fprintf(&sb, " · %d image(s) in thread", imageCount)
	}
	sb.WriteString("_\n\n")
	sb.WriteString(quote(slackx.Escape(p.Body)))
	sb.WriteString("\n\n_Reply in this thread to answer. Text and images are sent back to the visitor._")
	return sb.String()
}

func safeHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func (w *Worker) deliver(ctx context.Context, p *store.PendingQuery) {
	err := w.deliverOnce(ctx, p)
	if err == nil {
		slog.Info("query delivered to slack", "id", p.ID)
		return
	}
	retry := backoff(p.Attempts)
	var rl *slackx.RateLimitedError
	if errors.As(err, &rl) && rl.RetryAfter > retry {
		retry = rl.RetryAfter
	}
	slog.Error("slack delivery failed", "id", p.ID, "attempt", p.Attempts+1, "retry_in", retry.String(), "err", err)
	if mErr := w.o.Store.MarkFailed(context.WithoutCancel(ctx), p.ID, err.Error(), retry); mErr != nil {
		slog.Error("could not record failure", "id", p.ID, "err", mErr)
	}
}

func (w *Worker) deliverOnce(ctx context.Context, p *store.PendingQuery) error {
	files, err := w.o.Store.UnsentFiles(ctx, p.ID)
	if err != nil {
		return err
	}
	ts := p.SlackTS
	if ts == "" {
		if ts, err = w.o.Slack.PostRoot(ctx, RootText(p, len(files))); err != nil {
			return fmt.Errorf("post message: %w", err)
		}
		// Record the thread immediately so a failure below never re-posts the root.
		if err := w.o.Store.SetRootPosted(ctx, p.ID, ts); err != nil {
			return fmt.Errorf("save thread: %w", err)
		}
	}
	for _, f := range files {
		name := fmt.Sprintf("%s-image-%d.%s", p.ID, f.Position+1, f.Ext)
		if err := w.o.Slack.UploadImage(ctx, ts, name, f.Data); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		if err := w.o.Store.MarkFileSent(ctx, f.ID); err != nil {
			return err
		}
	}
	return w.o.Store.MarkDone(ctx, p.ID)
}

// FollowUpText builds the Slack message for a visitor follow-up.
func FollowUpText(queryID, body string, imageCount int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, ":speech_balloon: *Follow-up from the visitor* on `%s`", queryID)
	if imageCount > 0 {
		fmt.Fprintf(&sb, " _(%d image(s) below)_", imageCount)
	}
	sb.WriteString("\n\n")
	sb.WriteString(quote(slackx.Escape(body)))
	return sb.String()
}

func (w *Worker) deliverFollowUp(ctx context.Context, f *store.PendingFollowUp) {
	err := w.deliverFollowUpOnce(ctx, f)
	if err == nil {
		slog.Info("follow-up delivered to slack", "id", f.QueryID)
		return
	}
	retry := backoff(f.Attempts)
	var rl *slackx.RateLimitedError
	if errors.As(err, &rl) && rl.RetryAfter > retry {
		retry = rl.RetryAfter
	}
	slog.Error("follow-up delivery failed", "id", f.QueryID, "attempt", f.Attempts+1, "retry_in", retry.String(), "err", err)
	if mErr := w.o.Store.MarkFollowUpFailed(context.WithoutCancel(ctx), f.ID, err.Error(), retry); mErr != nil {
		slog.Error("could not record failure", "id", f.QueryID, "err", mErr)
	}
}

func (w *Worker) deliverFollowUpOnce(ctx context.Context, f *store.PendingFollowUp) error {
	files, err := w.o.Store.UnsentReplyFiles(ctx, f.ID)
	if err != nil {
		return err
	}
	if f.SlackTS == "" {
		ts, err := w.o.Slack.PostNote(ctx, f.ThreadTS, FollowUpText(f.QueryID, f.Body, len(files)))
		if err != nil {
			return fmt.Errorf("post follow-up: %w", err)
		}
		if err := w.o.Store.SetFollowUpPosted(ctx, f.ID, ts); err != nil {
			return fmt.Errorf("save follow-up: %w", err)
		}
	}
	for _, uf := range files {
		name := fmt.Sprintf("%s-followup-%d-image-%d.%s", f.QueryID, f.ID, uf.Position+1, uf.Ext)
		if err := w.o.Slack.UploadImage(ctx, f.ThreadTS, name, uf.Data); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		if err := w.o.Store.MarkFileSent(ctx, uf.ID); err != nil {
			return err
		}
	}
	return w.o.Store.MarkFollowUpDone(ctx, f.ID)
}

// backoff grows 10s, 20s, 40s ... capped at 15 minutes. Queries are never dropped.
func backoff(attempts int) time.Duration {
	d := 10 * time.Second
	for i := 0; i < attempts && d < 15*time.Minute; i++ {
		d *= 2
	}
	return min(d, 15*time.Minute)
}

// ---------- sync (Slack -> visitor) ----------

func (w *Worker) syncLoop(ctx context.Context) {
	w.reconcile(ctx) // catch up after a restart
	t := time.NewTicker(w.o.ReconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ts := <-w.syncQ:
			w.mu.Lock()
			delete(w.queued, ts)
			w.mu.Unlock()
			w.syncByTS(ctx, ts)
		case <-t.C:
			w.reconcile(ctx)
		}
	}
}

func (w *Worker) reconcile(ctx context.Context) {
	threads, err := w.o.Store.RecentThreads(ctx, time.Now().Add(-w.o.ReconcileAge))
	if err != nil {
		slog.Error("reconcile: listing threads failed", "err", err)
		return
	}
	for _, t := range threads {
		if ctx.Err() != nil {
			return
		}
		if err := w.SyncThread(ctx, t.QueryID, t.SlackTS); err != nil {
			slog.Warn("reconcile: sync failed", "id", t.QueryID, "err", err)
		}
		select { // stay far below Slack's rate limits
		case <-ctx.Done():
			return
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

func (w *Worker) syncByTS(ctx context.Context, threadTS string) {
	id, err := w.o.Store.QueryBySlackTS(ctx, threadTS)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) { // threads of other messages are ignored
			slog.Error("sync: lookup failed", "err", err)
		}
		return
	}
	if err := w.SyncThread(ctx, id, threadTS); err != nil {
		slog.Warn("sync failed, the periodic reconcile will retry", "id", id, "err", err)
	}
}

// SyncThread stores any new replies from the owner in the thread. It is
// idempotent: replies are keyed by their Slack timestamp.
func (w *Worker) SyncThread(ctx context.Context, queryID, threadTS string) error {
	msgs, err := w.o.Slack.Thread(ctx, threadTS)
	if err != nil {
		return err
	}
	known, err := w.o.Store.KnownReplies(ctx, queryID)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if m.TS == threadTS || known[m.TS] {
			continue
		}
		// Only the owner's own messages count: the bot's posts, other members
		// and system messages are never shown to visitors.
		if m.User != w.o.OwnerID || m.BotID != "" || (m.SubType != "" && m.SubType != "file_share") {
			continue
		}
		if err := w.storeReply(ctx, queryID, threadTS, m); err != nil {
			return fmt.Errorf("reply %s: %w", m.TS, err)
		}
	}
	return nil
}

func (w *Worker) storeReply(ctx context.Context, queryID, threadTS string, m slackx.ThreadMessage) error {
	var imgs []store.Image
	var ignored []string
	for _, f := range m.Files {
		if !strings.HasPrefix(f.MIME, "image/") || len(imgs) >= w.o.MaxImages {
			ignored = append(ignored, f.Name)
			continue
		}
		if f.Size > w.o.MaxImageBytes {
			ignored = append(ignored, f.Name+" (too large)")
			continue
		}
		data, err := w.o.Slack.Download(ctx, f, int64(w.o.MaxImageBytes))
		if errors.Is(err, slackx.ErrTooLarge) {
			ignored = append(ignored, f.Name+" (too large)")
			continue
		}
		if err != nil {
			return fmt.Errorf("download %s: %w", f.Name, err) // transient: retry later, store nothing
		}
		clean, info, err := images.Check(data)
		if err != nil {
			ignored = append(ignored, f.Name)
			continue
		}
		imgs = append(imgs, store.Image{MIME: info.MIME, Ext: info.Ext, Data: clean})
	}
	text := slackx.ToPlain(m.Text)
	added, err := w.o.Store.AddReply(ctx, store.NewReply{
		QueryID: queryID, SlackTS: m.TS, At: slackx.TSToTime(m.TS),
		Body: text, Hidden: text == "" && len(imgs) == 0, Images: imgs,
	})
	if err != nil {
		return err
	}
	if added {
		slog.Info("reply stored", "id", queryID, "images", len(imgs))
		if len(ignored) > 0 {
			note := ":warning: Not sent to the visitor (only images up to the size limit are supported): " +
				slackx.Escape(strings.Join(ignored, ", "))
			if _, err := w.o.Slack.PostNote(ctx, threadTS, note); err != nil {
				slog.Warn("could not post note", "err", err)
			}
		}
	}
	return nil
}
