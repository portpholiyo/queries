// Package server contains the HTTP API for submitting and tracking queries.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/portpholiyo/queries/internal/captcha"
	"github.com/portpholiyo/queries/internal/images"
	"github.com/portpholiyo/queries/internal/queryid"
	"github.com/portpholiyo/queries/internal/store"
)

// Store is the persistence the server needs (implemented by *store.Store).
type Store interface {
	CreateQuery(ctx context.Context, q store.NewQuery) error
	CountToday(ctx context.Context) (int, error)
	AddFollowUp(ctx context.Context, queryID, ipHash, body string, images []store.Image, perThread, perWindow int) (int64, error)
	CountFollowUpsToday(ctx context.Context) (int, error)
	GetThread(ctx context.Context, id string) (*store.Thread, error)
	GetFile(ctx context.Context, queryID, fileID string) (string, []byte, error)
	Ping(ctx context.Context) error
}

// Options configures a Server.
type Options struct {
	Store           Store
	Captcha         captcha.Verifier
	Notify          func() // called after a query is stored
	IPSalt          string
	AllowedOrigins  []string
	ClientIPHeader  string
	DailyCap        int
	FollowUpCap     int // follow-ups accepted per day, all threads together
	MaxFollowUps    int // follow-ups per thread, overall
	FollowUpWindow  int // follow-ups per thread within 10 minutes
	MaxMessageChars int
	MaxImages       int
	MaxImageBytes   int
	MaxConcurrent   int // simultaneous submissions being processed (bounds memory)
	SlackStatus     func() bool
}

// Server is the HTTP handler.
type Server struct {
	opts    Options
	submit  *ipLimiter
	follow  *ipLimiter
	track   *ipLimiter
	sem     chan struct{}
	origins map[string]struct{}
	mux     *http.ServeMux
}

// New builds the server; ctx bounds background goroutines.
func New(ctx context.Context, o Options) *Server {
	if o.MaxConcurrent < 1 {
		o.MaxConcurrent = 6
	}
	if o.FollowUpCap < 1 {
		o.FollowUpCap = 300
	}
	if o.MaxFollowUps < 1 {
		o.MaxFollowUps = 30
	}
	if o.FollowUpWindow < 1 {
		o.FollowUpWindow = 5
	}
	s := &Server{
		opts: o,
		// 1 submission per 2 minutes sustained, bursts of 3; 1 lookup per 2s, bursts of 15.
		submit:  newIPLimiter(ctx, 2*time.Minute, 3),
		follow:  newIPLimiter(ctx, 20*time.Second, 5),
		track:   newIPLimiter(ctx, 2*time.Second, 15),
		sem:     make(chan struct{}, o.MaxConcurrent),
		origins: make(map[string]struct{}, len(o.AllowedOrigins)),
		mux:     http.NewServeMux(),
	}
	for _, origin := range o.AllowedOrigins {
		s.origins[origin] = struct{}{}
	}
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("POST /api/query", s.create)
	s.mux.HandleFunc("POST /api/query/{id}/replies", s.followUp)
	s.mux.HandleFunc("GET /api/query/{id}", s.get)
	s.mux.HandleFunc("GET /api/query/{id}/files/{file}", s.file)
	return s
}

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler { return s.recoverer(s.logRequests(s.cors(s.mux))) }

// ---------- middleware ----------

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in handler", "panic", rec, "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int)        { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		// The path carries the query id, which is a credential: log only its shape.
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/query/") {
			switch {
			case strings.Contains(path, "/files/"):
				path = "/api/query/{id}/files/{file}"
			case strings.HasSuffix(path, "/replies"):
				path = "/api/query/{id}/replies"
			default:
				path = "/api/query/{id}"
			}
		}
		slog.Info("request", "method", r.Method, "path", path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := s.origins[origin]; ok {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Vary", "Origin")
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type")
				h.Set("Access-Control-Max-Age", "600")
			} else if r.Method != http.MethodGet {
				writeJSON(w, http.StatusForbidden, errBody("origin not allowed"))
				return
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- handlers ----------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.opts.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("database unavailable"))
		return
	}
	// Slack being down does not make the service unhealthy: queries are queued.
	body := map[string]any{"status": "ok"}
	if s.opts.SlackStatus != nil {
		body["slack_connected"] = s.opts.SlackStatus()
	}
	writeJSON(w, http.StatusOK, body)
}

type createResponse struct {
	ID string `json:"id"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.submit.Allow(ip) {
		w.Header().Set("Retry-After", "120")
		writeJSON(w, http.StatusTooManyRequests, errBody("You're sending queries too quickly. Please wait a couple of minutes."))
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, errBody("Busy right now. Please try again in a moment."))
		return
	}

	maxBody := int64(s.opts.MaxImages*s.opts.MaxImageBytes) + 256<<10
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	f, err := s.readForm(r)
	if err != nil {
		s.formError(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ok, err := s.opts.Captcha.Verify(ctx, f.captcha, ip)
	if err != nil {
		slog.Error("captcha unavailable", "err", err)
		writeJSON(w, http.StatusBadGateway, errBody("Could not verify you're human right now. Please try again."))
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, errBody("Human check failed. Please reload the page and try again."))
		return
	}
	if n, err := s.opts.Store.CountToday(ctx); err != nil {
		slog.Error("count failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
		return
	} else if n >= s.opts.DailyCap {
		writeJSON(w, http.StatusTooManyRequests, errBody("Too many queries today. Please try again tomorrow."))
		return
	}

	id := queryid.New()
	err = s.opts.Store.CreateQuery(ctx, store.NewQuery{
		ID: id, IPHash: s.hashIP(ip), UserAgent: truncate(r.UserAgent(), 300), Body: f.message, Images: f.images,
	})
	if err != nil {
		slog.Error("create query failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
		return
	}
	slog.Info("query received", "images", len(f.images), "chars", utf8.RuneCountInString(f.message))
	if s.opts.Notify != nil {
		s.opts.Notify()
	}
	writeJSON(w, http.StatusCreated, createResponse{ID: id})
}

// followUp stores a visitor's reply to their own query and queues it for Slack.
func (s *Server) followUp(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.follow.Allow(ip) {
		w.Header().Set("Retry-After", "20")
		writeJSON(w, http.StatusTooManyRequests, errBody("You're replying too quickly. Please wait a moment."))
		return
	}
	id, ok := queryid.Normalize(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("We couldn't find that query ID."))
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, errBody("Busy right now. Please try again in a moment."))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.opts.MaxImages*s.opts.MaxImageBytes)+256<<10)
	f, err := s.readForm(r)
	if err != nil {
		s.formError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ok, err = s.opts.Captcha.Verify(ctx, f.captcha, ip)
	if err != nil {
		slog.Error("captcha unavailable", "err", err)
		writeJSON(w, http.StatusBadGateway, errBody("Could not verify you're human right now. Please try again."))
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, errBody("Human check failed. Please reload the page and try again."))
		return
	}
	if n, err := s.opts.Store.CountFollowUpsToday(ctx); err != nil {
		slog.Error("count follow-ups failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
		return
	} else if n >= s.opts.FollowUpCap {
		writeJSON(w, http.StatusTooManyRequests, errBody("Too many replies today. Please try again tomorrow."))
		return
	}
	_, err = s.opts.Store.AddFollowUp(ctx, id, s.hashIP(ip), f.message, f.images, s.opts.MaxFollowUps, s.opts.FollowUpWindow)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody("We couldn't find that query ID."))
		return
	case errors.Is(err, store.ErrFollowUpLimit):
		writeJSON(w, http.StatusTooManyRequests, errBody("You've sent a lot of replies on this query. Please wait a few minutes."))
		return
	case err != nil:
		slog.Error("add follow-up failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
		return
	}
	slog.Info("follow-up received", "images", len(f.images), "chars", utf8.RuneCountInString(f.message))
	if s.opts.Notify != nil {
		s.opts.Notify()
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "sent"})
}

// formError maps readForm errors to responses.
func (s *Server) formError(w http.ResponseWriter, err error) {
	var ue userError
	if errors.As(err, &ue) {
		writeJSON(w, ue.status, errBody(ue.msg))
		return
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody("The upload is too large."))
		return
	}
	writeJSON(w, http.StatusBadRequest, errBody("Invalid request."))
}

type userError struct {
	status int
	msg    string
}

func (e userError) Error() string { return e.msg }

func bad(msg string) error { return userError{http.StatusBadRequest, msg} }

type form struct {
	message string
	captcha string
	images  []store.Image
}

// readForm streams the multipart body, enforcing every limit.
func (s *Server) readForm(r *http.Request) (*form, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, bad("Invalid request.")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	f := &form{}
	for parts := 0; ; parts++ {
		if parts > s.opts.MaxImages+8 {
			return nil, bad("Invalid request.")
		}
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch p.FormName() {
		case "message":
			b, err := io.ReadAll(io.LimitReader(p, int64(s.opts.MaxMessageChars)*4+1))
			if err != nil {
				return nil, err
			}
			f.message = string(b)
		case "captcha-response":
			b, err := io.ReadAll(io.LimitReader(p, 4096))
			if err != nil {
				return nil, err
			}
			f.captcha = string(b)
		case "images":
			if p.FileName() == "" {
				continue // an empty file input
			}
			if len(f.images) >= s.opts.MaxImages {
				return nil, bad(fmt.Sprintf("You can attach up to %d images.", s.opts.MaxImages))
			}
			data, err := io.ReadAll(io.LimitReader(p, int64(s.opts.MaxImageBytes)+1))
			if err != nil {
				return nil, err
			}
			if len(data) > s.opts.MaxImageBytes {
				return nil, bad(fmt.Sprintf("Each image must be under %d MB.", s.opts.MaxImageBytes>>20))
			}
			if len(data) == 0 {
				continue
			}
			clean, info, err := images.Check(data)
			if err != nil {
				return nil, bad("Only image files are supported (PNG, JPEG, GIF, WebP, AVIF, HEIC, BMP, TIFF).")
			}
			f.images = append(f.images, store.Image{MIME: info.MIME, Ext: info.Ext, Data: clean})
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(p, 1<<10))
		}
	}
	msg, err := s.cleanMessage(f.message)
	if err != nil {
		return nil, err
	}
	f.message = msg
	return f, nil
}

func (s *Server) cleanMessage(m string) (string, error) {
	if !utf8.ValidString(m) {
		return "", bad("Invalid text encoding.")
	}
	m = strings.TrimSpace(strings.ReplaceAll(m, "\r\n", "\n"))
	if strings.ContainsRune(m, 0) {
		return "", bad("Invalid characters in the message.")
	}
	n := utf8.RuneCountInString(m)
	if n == 0 {
		return "", bad("Please write your query.")
	}
	if n > s.opts.MaxMessageChars {
		return "", bad(fmt.Sprintf("Please keep your query under %d characters.", s.opts.MaxMessageChars))
	}
	return m, nil
}

type imageJSON struct {
	URL    string `json:"url"`
	MIME   string `json:"mime"`
	Inline bool   `json:"inline"`
}

type messageJSON struct {
	From   string      `json:"from"` // "you" or "owner"
	At     time.Time   `json:"at"`
	Text   string      `json:"text"`
	Images []imageJSON `json:"images"`
}

type threadJSON struct {
	ID       string        `json:"id"`
	Status   string        `json:"status"` // "waiting" or "replied"
	Messages []messageJSON `json:"messages"`
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	id, ok := queryid.Normalize(r.PathValue("id"))
	if !s.track.Allow(s.clientIP(r)) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, errBody("Too many requests. Please slow down."))
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("We couldn't find that query ID."))
		return
	}
	t, err := s.opts.Store.GetThread(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errBody("We couldn't find that query ID."))
		return
	}
	if err != nil {
		slog.Error("get thread failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("Something went wrong. Please try again."))
		return
	}
	out := threadJSON{ID: t.ID, Status: "waiting", Messages: make([]messageJSON, 0, len(t.Messages))}
	for _, m := range t.Messages {
		mj := messageJSON{From: "you", At: m.At, Text: m.Text, Images: []imageJSON{}}
		if m.FromOwner {
			mj.From = "owner"
		}
		for _, f := range m.Files {
			mj.Images = append(mj.Images, imageJSON{
				URL:    "/api/query/" + t.ID + "/files/" + f.ID,
				MIME:   f.MIME,
				Inline: inlineMIME(f.MIME),
			})
		}
		out.Messages = append(out.Messages, mj)
	}
	// "replied" while the last word is the owner's; back to "waiting" after a follow-up.
	if n := len(t.Messages); n > 0 && t.Messages[n-1].FromOwner {
		out.Status = "replied"
	}
	writeJSON(w, http.StatusOK, out)
}

func inlineMIME(m string) bool {
	switch m {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp":
		return true
	}
	return false
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	if !s.track.Allow(s.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, errBody("Too many requests. Please slow down."))
		return
	}
	id, ok := queryid.Normalize(r.PathValue("id"))
	if !ok || len(r.PathValue("file")) != 36 {
		http.NotFound(w, r)
		return
	}
	mt, data, err := s.opts.Store.GetFile(r.Context(), id, r.PathValue("file"))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("get file failed", "err", err)
		}
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mt)
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if inlineMIME(mt) {
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Disposition", "attachment")
	}
	h.Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

// ---------- helpers ----------

func (s *Server) clientIP(r *http.Request) string {
	if h := s.opts.ClientIPHeader; h != "" {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			if ip := net.ParseIP(v); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) hashIP(ip string) string {
	mac := hmac.New(sha256.New, []byte(s.opts.IPSalt))
	mac.Write([]byte(ip))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
