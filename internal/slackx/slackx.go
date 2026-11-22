// Package slackx is a thin wrapper around the Slack API with just what this
// service needs. The worker depends on the Client interface so it can be tested.
package slackx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// File is a file attached to a Slack message.
type File struct {
	ID       string
	Name     string
	MIME     string
	Size     int
	Download string // private download URL
}

// ThreadMessage is one message of a Slack thread.
type ThreadMessage struct {
	TS      string
	User    string
	BotID   string
	SubType string
	Text    string
	Files   []File
}

// RateLimitedError says how long Slack wants us to wait.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return "slack rate limited, retry after " + e.RetryAfter.String()
}

// Client is the Slack surface used by the worker.
type Client interface {
	// PostRoot posts the top-level message of a query and returns its timestamp.
	PostRoot(ctx context.Context, text string) (string, error)
	// PostNote posts a message into a thread and returns its timestamp.
	PostNote(ctx context.Context, threadTS, text string) (string, error)
	// UploadImage uploads an image into a thread.
	UploadImage(ctx context.Context, threadTS, filename string, data []byte) error
	// Thread returns every message of a thread, root first.
	Thread(ctx context.Context, threadTS string) ([]ThreadMessage, error)
	// Download fetches a Slack-hosted file, failing if it exceeds max bytes.
	Download(ctx context.Context, f File, max int64) ([]byte, error)
}

// API implements Client with the real Slack API.
type API struct {
	api     *slack.Client
	channel string
	token   string
	http    *http.Client
}

// New returns a Slack API client for one channel.
func New(api *slack.Client, botToken, channel string) *API {
	return &API{api: api, channel: channel, token: botToken, http: &http.Client{Timeout: 30 * time.Second}}
}

func wrap(err error) error {
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		return &RateLimitedError{RetryAfter: rl.RetryAfter}
	}
	return err
}

func (a *API) PostRoot(ctx context.Context, text string) (string, error) {
	_, ts, err := a.api.PostMessageContext(ctx, a.channel,
		slack.MsgOptionText(text, false), slack.MsgOptionDisableLinkUnfurl(), slack.MsgOptionDisableMediaUnfurl())
	return ts, wrap(err)
}

func (a *API) PostNote(ctx context.Context, threadTS, text string) (string, error) {
	_, ts, err := a.api.PostMessageContext(ctx, a.channel,
		slack.MsgOptionText(text, false), slack.MsgOptionTS(threadTS), slack.MsgOptionDisableLinkUnfurl())
	return ts, wrap(err)
}

func (a *API) UploadImage(ctx context.Context, threadTS, filename string, data []byte) error {
	_, err := a.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Reader:          bytes.NewReader(data),
		FileSize:        len(data),
		Filename:        filename,
		Title:           filename,
		Channel:         a.channel,
		ThreadTimestamp: threadTS,
	})
	return wrap(err)
}

func (a *API) Thread(ctx context.Context, threadTS string) ([]ThreadMessage, error) {
	var out []ThreadMessage
	cursor := ""
	for page := 0; page < 20; page++ {
		msgs, more, next, err := a.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: a.channel, Timestamp: threadTS, Cursor: cursor, Limit: 100,
		})
		if err != nil {
			return nil, wrap(err)
		}
		for _, m := range msgs {
			tm := ThreadMessage{TS: m.Timestamp, User: m.User, BotID: m.BotID, SubType: m.SubType, Text: m.Text}
			for _, f := range m.Files {
				dl := f.URLPrivateDownload
				if dl == "" {
					dl = f.URLPrivate
				}
				tm.Files = append(tm.Files, File{ID: f.ID, Name: f.Name, MIME: f.Mimetype, Size: f.Size, Download: dl})
			}
			out = append(out, tm)
		}
		if !more || next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

func (a *API) Download(ctx context.Context, f File, max int64) ([]byte, error) {
	u, err := url.Parse(f.Download)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".slack.com") {
		return nil, fmt.Errorf("refusing to download from %q", f.Download)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

// ErrTooLarge means a file exceeded the allowed size.
var ErrTooLarge = errors.New("file too large")

// Escape makes visitor text safe for Slack: no mentions, links or formatting
// tricks (only &, < and > are special in Slack's message format).
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ToPlain converts Slack message markup to plain text for the visitor:
// <https://x|label> becomes "label (https://x)", user/channel mentions are
// dropped, and HTML entities are decoded.
func ToPlain(s string) string {
	var sb strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			break
		}
		sb.WriteString(s[:i])
		tok := s[i+1 : i+j]
		s = s[i+j+1:]
		switch {
		case strings.HasPrefix(tok, "@"), strings.HasPrefix(tok, "!"):
			// mention or special mention: drop
		case strings.HasPrefix(tok, "#"):
			if _, name, ok := strings.Cut(tok, "|"); ok {
				sb.WriteString("#" + name)
			}
		default:
			link, label, ok := strings.Cut(tok, "|")
			switch {
			case strings.HasPrefix(link, "mailto:"):
				sb.WriteString(strings.TrimPrefix(link, "mailto:"))
			case ok && label != "" && label != link:
				sb.WriteString(label + " (" + link + ")")
			default:
				sb.WriteString(link)
			}
		}
	}
	sb.WriteString(s)
	out := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(sb.String())
	return strings.TrimSpace(out)
}

// TSToTime parses a Slack timestamp such as "1700000000.000200".
func TSToTime(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Now()
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		ns, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(s, ns).UTC()
}
