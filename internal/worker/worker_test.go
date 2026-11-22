package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/portpholiyo/queries/internal/slackx"
	"github.com/portpholiyo/queries/internal/store"
)

const owner = "UOWNER000"

var png = []byte("\x89PNG\r\n\x1a\n-pixels-")

type fakeStore struct {
	mu      sync.Mutex
	pending *store.PendingQuery
	files   []store.UnsentFile
	root    string
	done    bool
	failed  []string
	replies map[string]store.NewReply

	followUp     *store.PendingFollowUp
	followFiles  []store.UnsentFile
	followTS     string
	followDone   bool
	followFailed []string
}

func (f *fakeStore) ClaimFollowUp(context.Context, time.Duration) (*store.PendingFollowUp, error) {
	if f.followUp == nil || f.followDone {
		return nil, nil
	}
	p := *f.followUp
	p.SlackTS = f.followTS
	return &p, nil
}
func (f *fakeStore) SetFollowUpPosted(_ context.Context, _ int64, ts string) error {
	f.followTS = ts
	return nil
}
func (f *fakeStore) UnsentReplyFiles(context.Context, int64) ([]store.UnsentFile, error) {
	return append([]store.UnsentFile(nil), f.followFiles...), nil
}
func (f *fakeStore) MarkFollowUpDone(context.Context, int64) error { f.followDone = true; return nil }
func (f *fakeStore) MarkFollowUpFailed(_ context.Context, _ int64, msg string, _ time.Duration) error {
	f.followFailed = append(f.followFailed, msg)
	return nil
}

func (f *fakeStore) ClaimPending(context.Context, time.Duration) (*store.PendingQuery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending == nil || f.done {
		return nil, nil
	}
	p := *f.pending
	p.SlackTS = f.root
	return &p, nil
}
func (f *fakeStore) SetRootPosted(_ context.Context, _, ts string) error { f.root = ts; return nil }
func (f *fakeStore) UnsentFiles(context.Context, string) ([]store.UnsentFile, error) {
	return append([]store.UnsentFile(nil), f.files...), nil
}
func (f *fakeStore) MarkFileSent(_ context.Context, id string) error {
	for i, x := range f.files {
		if x.ID == id {
			f.files = append(f.files[:i], f.files[i+1:]...)
			return nil
		}
	}
	for i, x := range f.followFiles {
		if x.ID == id {
			f.followFiles = append(f.followFiles[:i], f.followFiles[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeStore) MarkDone(context.Context, string) error { f.done = true; return nil }
func (f *fakeStore) MarkFailed(_ context.Context, _, msg string, _ time.Duration) error {
	f.failed = append(f.failed, msg)
	return nil
}
func (f *fakeStore) RecentThreads(context.Context, time.Time) ([]store.ThreadRef, error) {
	return nil, nil
}
func (f *fakeStore) QueryBySlackTS(context.Context, string) (string, error) { return "Q-1", nil }
func (f *fakeStore) KnownReplies(context.Context, string) (map[string]bool, error) {
	out := map[string]bool{}
	for k := range f.replies {
		out[k] = true
	}
	return out, nil
}
func (f *fakeStore) AddReply(_ context.Context, r store.NewReply) (bool, error) {
	if f.replies == nil {
		f.replies = map[string]store.NewReply{}
	}
	if _, ok := f.replies[r.SlackTS]; ok {
		return false, nil
	}
	f.replies[r.SlackTS] = r
	return true, nil
}

type fakeSlack struct {
	posts     []string
	uploads   []string
	notes     []string
	thread    []slackx.ThreadMessage
	failPost  error
	failUpTo  int // fail this many uploads first
	downloads map[string][]byte
	dlErr     error
}

func (s *fakeSlack) PostRoot(_ context.Context, text string) (string, error) {
	if s.failPost != nil {
		return "", s.failPost
	}
	s.posts = append(s.posts, text)
	return "100.1", nil
}
func (s *fakeSlack) PostNote(_ context.Context, _, text string) (string, error) {
	s.notes = append(s.notes, text)
	return "200.1", nil
}
func (s *fakeSlack) UploadImage(_ context.Context, _, name string, _ []byte) error {
	if s.failUpTo > 0 {
		s.failUpTo--
		return errors.New("upload boom")
	}
	s.uploads = append(s.uploads, name)
	return nil
}
func (s *fakeSlack) Thread(context.Context, string) ([]slackx.ThreadMessage, error) {
	return s.thread, nil
}
func (s *fakeSlack) Download(_ context.Context, f slackx.File, _ int64) ([]byte, error) {
	if s.dlErr != nil {
		return nil, s.dlErr
	}
	return s.downloads[f.ID], nil
}

func newWorker(st *fakeStore, sl *fakeSlack) *Worker {
	return New(Options{Store: st, Slack: sl, OwnerID: owner, MaxImages: 3, MaxImageBytes: 1 << 20})
}

func TestDeliverPostsRootThenImages(t *testing.T) {
	st := &fakeStore{
		pending: &store.PendingQuery{ID: "Q-AAAAA-BBBBB", CreatedAt: time.Now(), IPHash: "abcdef0123456789", Body: "hi <!channel> & bye"},
		files:   []store.UnsentFile{{ID: "f1", Position: 0, Ext: "png", Data: png}, {ID: "f2", Position: 1, Ext: "jpg", Data: png}},
	}
	sl := &fakeSlack{}
	w := newWorker(st, sl)
	p, _ := st.ClaimPending(context.Background(), time.Minute)
	w.deliver(context.Background(), p)
	if !st.done || len(sl.posts) != 1 || len(sl.uploads) != 2 {
		t.Fatalf("done=%v posts=%d uploads=%v", st.done, len(sl.posts), sl.uploads)
	}
	if strings.Contains(sl.posts[0], "<!channel>") || !strings.Contains(sl.posts[0], "&lt;!channel&gt;") {
		t.Errorf("visitor text was not escaped: %q", sl.posts[0])
	}
	if !strings.Contains(sl.posts[0], "Q-AAAAA-BBBBB") {
		t.Error("query id missing from slack message")
	}
}

func TestDeliverRetryDoesNotRepostRoot(t *testing.T) {
	st := &fakeStore{
		pending: &store.PendingQuery{ID: "Q-1", CreatedAt: time.Now(), Body: "x"},
		files:   []store.UnsentFile{{ID: "f1", Ext: "png", Data: png}},
	}
	sl := &fakeSlack{failUpTo: 1}
	w := newWorker(st, sl)
	for i := 0; i < 2; i++ {
		p, _ := st.ClaimPending(context.Background(), time.Minute)
		if p == nil {
			break
		}
		w.deliver(context.Background(), p)
	}
	if len(sl.posts) != 1 {
		t.Errorf("root posted %d times, want exactly once", len(sl.posts))
	}
	if !st.done || len(sl.uploads) != 1 || len(st.failed) != 1 {
		t.Errorf("done=%v uploads=%v failed=%v", st.done, sl.uploads, st.failed)
	}
}

func TestDeliverFailureIsRecordedNotLost(t *testing.T) {
	st := &fakeStore{pending: &store.PendingQuery{ID: "Q-1", CreatedAt: time.Now(), Body: "x"}}
	sl := &fakeSlack{failPost: errors.New("slack down")}
	w := newWorker(st, sl)
	p, _ := st.ClaimPending(context.Background(), time.Minute)
	w.deliver(context.Background(), p)
	if st.done || len(st.failed) != 1 || st.root != "" {
		t.Errorf("done=%v failed=%v root=%q", st.done, st.failed, st.root)
	}
}

func TestBackoff(t *testing.T) {
	if backoff(0) != 10*time.Second || backoff(1) != 20*time.Second || backoff(50) != 15*time.Minute {
		t.Errorf("unexpected backoff: %v %v %v", backoff(0), backoff(1), backoff(50))
	}
}

func TestSyncOnlyOwnerRepliesAreStored(t *testing.T) {
	st := &fakeStore{}
	sl := &fakeSlack{
		downloads: map[string][]byte{"img": png, "evil": []byte("<svg onload=alert(1)>")},
		thread: []slackx.ThreadMessage{
			{TS: "100.1", User: "UBOT", BotID: "B1", Text: "root"},
			{TS: "100.2", User: "USTRANGER", Text: "I am not the owner"},
			{TS: "100.3", User: owner, BotID: "B1", Text: "bot posing as owner"},
			{TS: "100.4", User: owner, SubType: "channel_join", Text: "joined"},
			{TS: "100.5", User: owner, Text: "Thanks! See <https://x.dev|this> <@U1>"},
			{TS: "100.6", User: owner, SubType: "file_share", Text: "screenshot", Files: []slackx.File{
				{ID: "img", Name: "a.png", MIME: "image/png", Size: len(png)},
				{ID: "pdf", Name: "doc.pdf", MIME: "application/pdf", Size: 10},
				{ID: "evil", Name: "evil.png", MIME: "image/png", Size: 21},
			}},
		},
	}
	w := newWorker(st, sl)
	if err := w.SyncThread(context.Background(), "Q-1", "100.1"); err != nil {
		t.Fatal(err)
	}
	if len(st.replies) != 2 {
		t.Fatalf("stored %d replies, want 2: %+v", len(st.replies), st.replies)
	}
	if r := st.replies["100.5"]; r.Body != "Thanks! See this (https://x.dev)" {
		t.Errorf("reply text = %q", r.Body)
	}
	r := st.replies["100.6"]
	if len(r.Images) != 1 || r.Images[0].MIME != "image/png" {
		t.Errorf("images = %+v (the pdf and the spoofed svg must be dropped)", r.Images)
	}
	if len(sl.notes) != 1 || !strings.Contains(sl.notes[0], "doc.pdf") {
		t.Errorf("owner should be told what was skipped: %v", sl.notes)
	}
	// a second sync changes nothing and posts no new notes
	if err := w.SyncThread(context.Background(), "Q-1", "100.1"); err != nil || len(st.replies) != 2 || len(sl.notes) != 1 {
		t.Errorf("sync is not idempotent: %v %d %d", err, len(st.replies), len(sl.notes))
	}
}

func TestSyncTransientDownloadErrorStoresNothing(t *testing.T) {
	st := &fakeStore{}
	sl := &fakeSlack{
		dlErr: errors.New("network"),
		thread: []slackx.ThreadMessage{
			{TS: "100.1", User: "UBOT", BotID: "B1"},
			{TS: "100.2", User: owner, SubType: "file_share", Text: "pic", Files: []slackx.File{{ID: "img", Name: "a.png", MIME: "image/png", Size: 5}}},
		},
	}
	w := newWorker(st, sl)
	if err := w.SyncThread(context.Background(), "Q-1", "100.1"); err == nil {
		t.Fatal("expected an error so the sync is retried")
	}
	if len(st.replies) != 0 {
		t.Error("reply must not be stored when its image could not be fetched")
	}
	sl.dlErr, sl.downloads = nil, map[string][]byte{"img": png}
	if err := w.SyncThread(context.Background(), "Q-1", "100.1"); err != nil || len(st.replies) != 1 {
		t.Fatalf("retry failed: %v %d", err, len(st.replies))
	}
}

func TestOnlyUnsupportedFileIsHidden(t *testing.T) {
	st := &fakeStore{}
	sl := &fakeSlack{thread: []slackx.ThreadMessage{
		{TS: "100.1", BotID: "B1"},
		{TS: "100.2", User: owner, SubType: "file_share", Files: []slackx.File{{ID: "x", Name: "a.zip", MIME: "application/zip"}}},
	}}
	if err := newWorker(st, sl).SyncThread(context.Background(), "Q-1", "100.1"); err != nil {
		t.Fatal(err)
	}
	if r, ok := st.replies["100.2"]; !ok || !r.Hidden {
		t.Errorf("a reply with nothing to show should be recorded as hidden: %+v", r)
	}
}

func TestFollowUpDeliveredInThreadWithoutReposting(t *testing.T) {
	st := &fakeStore{
		followUp:    &store.PendingFollowUp{ID: 7, QueryID: "Q-AAAAA-BBBBB", ThreadTS: "100.1", Body: "one more thing <!channel>"},
		followFiles: []store.UnsentFile{{ID: "ff1", Ext: "png", Data: png}},
	}
	sl := &fakeSlack{failUpTo: 1} // the first image upload fails, so the follow-up is retried
	w := newWorker(st, sl)
	for i := 0; i < 2; i++ {
		f, _ := st.ClaimFollowUp(context.Background(), time.Minute)
		if f == nil {
			break
		}
		w.deliverFollowUp(context.Background(), f)
	}
	if len(sl.notes) != 1 {
		t.Fatalf("follow-up text posted %d times, want exactly once: %v", len(sl.notes), sl.notes)
	}
	if strings.Contains(sl.notes[0], "<!channel>") || !strings.Contains(sl.notes[0], "Q-AAAAA-BBBBB") {
		t.Errorf("follow-up text not escaped or missing id: %q", sl.notes[0])
	}
	if !st.followDone || len(sl.uploads) != 1 || len(st.followFailed) != 1 {
		t.Errorf("done=%v uploads=%v failed=%v", st.followDone, sl.uploads, st.followFailed)
	}
}
