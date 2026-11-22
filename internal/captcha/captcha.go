// Package captcha verifies CAPTCHA tokens against a provider's siteverify-style endpoint.
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrUnavailable means verification could not be performed (not a bad token).
var ErrUnavailable = errors.New("captcha unavailable")

// Verifier checks a token produced by the browser widget.
type Verifier interface {
	Verify(ctx context.Context, token, remoteIP string) (bool, error)
}

// Client talks to the provider's siteverify endpoint (form POST: secret, response, remoteip; JSON reply {"success": bool}).
type Client struct {
	secret string
	url    string
	http   *http.Client
}

// New returns a Client. The endpoint comes from CAPTCHA_VERIFY_URL; without it
// every verification fails closed with ErrUnavailable.
func New(secret string) *Client {
	return &Client{
		secret: secret,
		url:    os.Getenv("CAPTCHA_VERIFY_URL"),
		http:   &http.Client{Timeout: 8 * time.Second},
	}
}

// WithURL overrides the endpoint (tests).
func (c *Client) WithURL(u string) *Client { c.url = u; return c }

func (c *Client) Verify(ctx context.Context, token, remoteIP string) (bool, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 {
		return false, nil
	}
	if c.url == "" {
		return false, fmt.Errorf("%w: CAPTCHA_VERIFY_URL not set", ErrUnavailable)
	}
	form := url.Values{"secret": {c.secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return out.Success, nil
}
