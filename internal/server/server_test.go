package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/portpholiyo/queries/internal/store"
)

var png = []byte("\x89PNG\r\n\x1a\n-pixels-")

type fakeStore struct {
	mu      sync.Mutex
	created []store.NewQuery
	today   int
	thread  *store.Thread

	followUps []string
	followErr error
}

func (f *fakeStore) AddFollowUp(_ context.Context, id, _, body string, _ []store.Image, _, _ int) (int64, error) {
	if f.followErr != nil {
		return 0, f.followErr
	}
	if id != "Q-AAAAA-BBBBB" {
		return 0, store.ErrNotFound
	}
	f.followUps = append(f.followUps, body)
	return 1, nil
}
func (f *fakeStore) CountFollowUpsToday(context.Context) (int, error) { return 0, nil }

func (f *fakeStore) CreateQuery(_ context.Context, q store.NewQuery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, q)
	return nil
}
func (f *fakeStore) CountToday(context.Context) (int, error) { return f.today, nil }
func (f *fakeStore) GetThread(_ context.Context, id string) (*store.Thread, error) {
	if f.thread != nil && f.thread.ID == id {
		return f.thread, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeStore) GetFile(_ context.Context, q, file string) (string, []byte, error) {
	if q == "Q-AAAAA-BBBBB" && file == "11111111-1111-1111-1111-111111111111" {
		return "image/png", png, nil
	}
	return "", nil, store.ErrNotFound
}
func (f *fakeStore) Ping(context.Context) error { return nil }

type fakeCaptcha struct{ ok bool }

func (t fakeCaptcha) Verify(context.Context, string, string) (bool, error) { return t.ok, nil }

func newServer(t *testing.T, st *fakeStore, captcha bool) (*Server, *int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notified := 0
	return New(ctx, Options{
		Store: st, Captcha: fakeCaptcha{captcha}, Notify: func() { notified++ }, IPSalt: "0123456789abcdef",
		AllowedOrigins: []string{"https://example.com"}, ClientIPHeader: "X-Real-IP",
		DailyCap: 5, MaxMessageChars: 100, MaxImages: 2, MaxImageBytes: 1 << 10,
	}), &notified
}

type upload struct {
	message string
	files   [][]byte
}

func post(t *testing.T, s *Server, u upload, ip string) *httptest.ResponseRecorder {
	t.Helper()
	return postTo(t, s, "/api/query", u, ip)
}

func postTo(t *testing.T, s *Server, path string, u upload, ip string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("message", u.message)
	_ = mw.WriteField("captcha-response", "tok")
	for i, f := range u.files {
		fw, _ := mw.CreateFormFile("images", "pic"+string(rune('a'+i))+".png")
		_, _ = fw.Write(f)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("X-Real-IP", ip)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCreateSuccess(t *testing.T) {
	st := &fakeStore{}
	s, notified := newServer(t, st, true)
	rec := post(t, s, upload{"Hello there", [][]byte{png}}, "9.9.9.1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.ID, "Q-") || len(out.ID) != 13 {
		t.Errorf("bad id %q", out.ID)
	}
	if len(st.created) != 1 || st.created[0].Body != "Hello there" || len(st.created[0].Images) != 1 || *notified != 1 {
		t.Errorf("stored %+v notified=%d", st.created, *notified)
	}
	if st.created[0].IPHash == "" || strings.Contains(st.created[0].IPHash, "9.9.9.1") {
		t.Error("ip must be stored hashed")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Error("CORS header missing")
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		u    upload
		code int
	}{
		{"empty", upload{"   ", nil}, 400},
		{"too long", upload{strings.Repeat("a", 101), nil}, 400},
		{"too many images", upload{"x", [][]byte{png, png, png}}, 400},
		{"not an image", upload{"x", [][]byte{[]byte("<svg onload=alert(1)>")}}, 400},
		{"html as png", upload{"x", [][]byte{[]byte("<html></html>")}}, 400},
		{"image too big", upload{"x", [][]byte{append(append([]byte{}, png...), make([]byte, 2<<10)...)}}, 400},
	}
	for i, c := range cases {
		st := &fakeStore{}
		s, _ := newServer(t, st, true)
		rec := post(t, s, c.u, "9.9.8."+string(rune('1'+i)))
		if rec.Code != c.code || len(st.created) != 0 {
			t.Errorf("%s: status %d (stored %d): %s", c.name, rec.Code, len(st.created), rec.Body)
		}
	}
}

func TestCaptchaFailureAndDailyCap(t *testing.T) {
	st := &fakeStore{}
	s, _ := newServer(t, st, false)
	if rec := post(t, s, upload{"x", nil}, "9.9.7.1"); rec.Code != http.StatusForbidden || len(st.created) != 0 {
		t.Errorf("captcha failure: %d", rec.Code)
	}
	st = &fakeStore{today: 5}
	s, _ = newServer(t, st, true)
	if rec := post(t, s, upload{"x", nil}, "9.9.7.2"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("daily cap: %d", rec.Code)
	}
}

func TestSubmitRateLimitPerIP(t *testing.T) {
	st := &fakeStore{}
	s, _ := newServer(t, st, true)
	codes := []int{}
	for i := 0; i < 5; i++ {
		codes = append(codes, post(t, s, upload{"x", nil}, "9.9.6.1").Code)
	}
	if codes[0] != 201 || codes[3] != 429 {
		t.Errorf("codes = %v, want the 4th request limited", codes)
	}
	if post(t, s, upload{"x", nil}, "9.9.6.2").Code != 201 {
		t.Error("a different IP must not be limited")
	}
}

func TestForeignOriginRejectedOnPost(t *testing.T) {
	s, _ := newServer(t, &fakeStore{}, true)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(""))
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d", rec.Code)
	}
}

func TestGetThread(t *testing.T) {
	now := time.Now()
	st := &fakeStore{thread: &store.Thread{ID: "Q-AAAAA-BBBBB", Messages: []store.Message{
		{At: now, Text: "my question"},
		{FromOwner: true, At: now, Text: "my answer", Files: []store.FileRef{{ID: "11111111-1111-1111-1111-111111111111", MIME: "image/png"}}},
	}}}
	s, _ := newServer(t, st, true)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Real-IP", "8.8.8.8")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := get("/api/query/q-aaaaa-bbbbb") // lower case is accepted
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var out threadJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "replied" || len(out.Messages) != 2 || out.Messages[1].From != "owner" || len(out.Messages[1].Images) != 1 {
		t.Errorf("unexpected thread: %+v", out)
	}
	if strings.Contains(rec.Body.String(), "slack") {
		t.Error("response must not leak slack details")
	}
	st.thread.Messages = append(st.thread.Messages, store.Message{At: now, Text: "a follow-up"})
	rec = get("/api/query/Q-AAAAA-BBBBB")
	out = threadJSON{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "waiting" || len(out.Messages) != 3 || out.Messages[2].From != "you" {
		t.Errorf("after a visitor follow-up the status must be waiting: %+v", out)
	}
	if get("/api/query/Q-ZZZZZ-ZZZZZ").Code != 404 || get("/api/query/not-an-id").Code != 404 {
		t.Error("unknown or malformed ids must be 404")
	}
}

func TestFileHeaders(t *testing.T) {
	s, _ := newServer(t, &fakeStore{}, true)
	req := httptest.NewRequest(http.MethodGet, "/api/query/Q-AAAAA-BBBBB/files/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	h := rec.Header()
	if rec.Code != 200 || h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("Content-Disposition") != "inline" {
		t.Errorf("status %d headers %v", rec.Code, h)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/query/Q-AAAAA-BBBBB/files/22222222-2222-2222-2222-222222222222", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("unknown file: %d", rec.Code)
	}
}

func TestFollowUp(t *testing.T) {
	st := &fakeStore{}
	s, notified := newServer(t, st, true)
	rec := postTo(t, s, "/api/query/q-aaaaa-bbbbb/replies", upload{"thanks, one more question", [][]byte{png}}, "7.7.7.1")
	if rec.Code != http.StatusCreated || len(st.followUps) != 1 || *notified != 1 {
		t.Fatalf("status %d stored %v notified %d: %s", rec.Code, st.followUps, *notified, rec.Body)
	}
	if rec := postTo(t, s, "/api/query/Q-ZZZZZ-ZZZZZ/replies", upload{"hi", nil}, "7.7.7.2"); rec.Code != 404 {
		t.Errorf("unknown query: %d", rec.Code)
	}
	if rec := postTo(t, s, "/api/query/not-an-id/replies", upload{"hi", nil}, "7.7.7.3"); rec.Code != 404 {
		t.Errorf("malformed id: %d", rec.Code)
	}
	if rec := postTo(t, s, "/api/query/Q-AAAAA-BBBBB/replies", upload{"  ", nil}, "7.7.7.4"); rec.Code != 400 {
		t.Errorf("empty text: %d", rec.Code)
	}
	if rec := postTo(t, s, "/api/query/Q-AAAAA-BBBBB/replies", upload{"x", [][]byte{[]byte("<svg/>")}}, "7.7.7.5"); rec.Code != 400 {
		t.Errorf("non-image: %d", rec.Code)
	}
}

func TestFollowUpNeedsCaptchaAndRespectsQuota(t *testing.T) {
	st := &fakeStore{}
	s, _ := newServer(t, st, false)
	if rec := postTo(t, s, "/api/query/Q-AAAAA-BBBBB/replies", upload{"x", nil}, "7.7.6.1"); rec.Code != 403 || len(st.followUps) != 0 {
		t.Errorf("captcha: %d", rec.Code)
	}
	st = &fakeStore{followErr: store.ErrFollowUpLimit}
	s, _ = newServer(t, st, true)
	if rec := postTo(t, s, "/api/query/Q-AAAAA-BBBBB/replies", upload{"x", nil}, "7.7.6.2"); rec.Code != 429 {
		t.Errorf("quota: %d", rec.Code)
	}
}

func TestFollowUpRateLimitPerIP(t *testing.T) {
	st := &fakeStore{}
	s, _ := newServer(t, st, true)
	codes := []int{}
	for i := 0; i < 7; i++ {
		codes = append(codes, postTo(t, s, "/api/query/Q-AAAAA-BBBBB/replies", upload{"x", nil}, "7.7.5.1").Code)
	}
	if codes[4] != 201 || codes[5] != 429 {
		t.Errorf("codes = %v, want the 6th request limited", codes)
	}
}
