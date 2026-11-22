package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// Integration test: needs a PostgreSQL, e.g.
//
//	docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:19-alpine
//	TEST_DB_PORT=55432 TEST_DB_PASSWORD=pw go test ./internal/store
func open(t *testing.T) *Store {
	t.Helper()
	port := os.Getenv("TEST_DB_PORT")
	if port == "" {
		t.Skip("TEST_DB_PORT not set; skipping database integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, "127.0.0.1", port, "postgres", "postgres", os.Getenv("TEST_DB_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestQueryLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := "Q-TEST0-" + time.Now().Format("150405")[:5]
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")

	if err := s.CreateQuery(ctx, NewQuery{ID: id, IPHash: "h", Body: "hello", Images: []Image{{MIME: "image/png", Ext: "png", Data: png}}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountToday(ctx); err != nil || n < 1 {
		t.Fatalf("CountToday = %d, %v", n, err)
	}

	// delivery lease: claimed once, not again while leased
	var p *PendingQuery
	for i := 0; i < 20; i++ { // other test rows may exist; find ours
		q, err := s.ClaimPending(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if q == nil {
			break
		}
		if q.ID == id {
			p = q
			break
		}
	}
	if p == nil || p.Body != "hello" || p.SlackTS != "" {
		t.Fatalf("claim = %+v", p)
	}
	files, err := s.UnsentFiles(ctx, id)
	if err != nil || len(files) != 1 {
		t.Fatalf("unsent = %v, %v", files, err)
	}
	if err := s.SetRootPosted(ctx, id, "1700000000.000100"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFileSent(ctx, files[0].ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.UnsentFiles(ctx, id); len(left) != 0 {
		t.Error("file still unsent")
	}
	if err := s.MarkFailed(ctx, id, "boom", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, err := s.QueryBySlackTS(ctx, "1700000000.000100"); err != nil || got != id {
		t.Fatalf("QueryBySlackTS = %q, %v", got, err)
	}
	if _, err := s.QueryBySlackTS(ctx, "nope"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// replies are idempotent and respect visibility
	r := NewReply{QueryID: id, SlackTS: "1700000001.000100", At: time.Now(), Body: "answer", Images: []Image{{MIME: "image/jpeg", Ext: "jpg", Data: png}}}
	if added, err := s.AddReply(ctx, r); err != nil || !added {
		t.Fatalf("AddReply = %v, %v", added, err)
	}
	if added, err := s.AddReply(ctx, r); err != nil || added {
		t.Fatalf("duplicate AddReply = %v, %v (must be ignored)", added, err)
	}
	if _, err := s.AddReply(ctx, NewReply{QueryID: id, SlackTS: "1700000002.000100", At: time.Now(), Hidden: true}); err != nil {
		t.Fatal(err)
	}
	known, _ := s.KnownReplies(ctx, id)
	if len(known) != 2 {
		t.Errorf("known replies = %v", known)
	}

	th, err := s.GetThread(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) != 2 || th.Messages[0].Text != "hello" || len(th.Messages[0].Files) != 1 ||
		!th.Messages[1].FromOwner || th.Messages[1].Text != "answer" || len(th.Messages[1].Files) != 1 {
		t.Fatalf("thread = %+v", th)
	}
	mime, data, err := s.GetFile(ctx, id, th.Messages[1].Files[0].ID)
	if err != nil || mime != "image/jpeg" || string(data) != string(png) {
		t.Errorf("GetFile = %q %q %v", mime, data, err)
	}
	if _, _, err := s.GetFile(ctx, "Q-OTHER-QUERY", th.Messages[1].Files[0].ID); err != ErrNotFound {
		t.Errorf("a file must not be readable through another query id: %v", err)
	}
	if _, err := s.GetThread(ctx, "Q-NOPE0-NOPE0"); err != ErrNotFound {
		t.Errorf("unknown thread: %v", err)
	}
	if refs, err := s.RecentThreads(ctx, time.Now().Add(-time.Hour)); err != nil || len(refs) == 0 {
		t.Errorf("RecentThreads = %v, %v", refs, err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	s := open(t)
	if err := s.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFollowUpLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := "Q-FUP00-" + time.Now().Format("150405")[:5]
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")
	if err := s.CreateQuery(ctx, NewQuery{ID: id, IPHash: "h", Body: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFollowUp(ctx, "Q-NOPE0-NOPE0", "h", "x", nil, 5, 5); err != ErrNotFound {
		t.Errorf("unknown query: %v", err)
	}
	fid, err := s.AddFollowUp(ctx, id, "h", "second", []Image{{MIME: "image/png", Ext: "png", Data: png}}, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	// not deliverable until the Slack thread exists
	for i := 0; i < 50; i++ {
		f, err := s.ClaimFollowUp(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			break
		}
		if f.ID == fid {
			t.Fatal("follow-up claimed before its thread was posted")
		}
	}
	if err := s.SetRootPosted(ctx, id, "1800000000."+id[len(id)-5:]+"0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE queries.replies SET next_attempt_at = now() WHERE id = $1`, fid); err != nil {
		t.Fatal(err)
	}
	var got *PendingFollowUp
	for i := 0; i < 50 && got == nil; i++ {
		f, err := s.ClaimFollowUp(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if f == nil {
			break
		}
		if f.ID == fid {
			got = f
		}
	}
	if got == nil || got.Body != "second" || got.ThreadTS == "" || got.SlackTS != "" {
		t.Fatalf("claimed = %+v", got)
	}
	files, err := s.UnsentReplyFiles(ctx, fid)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %v, %v", files, err)
	}
	if err := s.SetFollowUpPosted(ctx, fid, "1800000001.000100"); err != nil {
		t.Fatal(err)
	}
	_ = s.MarkFileSent(ctx, files[0].ID)
	if err := s.MarkFollowUpDone(ctx, fid); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountFollowUpsToday(ctx); err != nil || n < 1 {
		t.Errorf("CountFollowUpsToday = %d, %v", n, err)
	}

	// shows up in the thread as the visitor's message, with its image, in order
	if _, err := s.AddReply(ctx, NewReply{QueryID: id, SlackTS: "1800000002.000100", At: time.Now(), Body: "owner answer"}); err != nil {
		t.Fatal(err)
	}
	th, err := s.GetThread(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) != 3 || th.Messages[1].FromOwner || th.Messages[1].Text != "second" || len(th.Messages[1].Files) != 1 ||
		!th.Messages[2].FromOwner {
		t.Fatalf("thread = %+v", th.Messages)
	}
	// the sync step must treat the bot's own follow-up post as already known
	if known, _ := s.KnownReplies(ctx, id); !known["1800000001.000100"] {
		t.Error("posted follow-up ts must be known")
	}

	// quotas
	if _, err := s.AddFollowUp(ctx, id, "h", "third", nil, 3, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFollowUp(ctx, id, "h", "fourth", nil, 3, 2); err != ErrFollowUpLimit {
		t.Errorf("window quota: %v", err)
	}
}
