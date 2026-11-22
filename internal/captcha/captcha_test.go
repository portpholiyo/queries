package captcha

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("secret") != "s3cret" {
			http.Error(w, "x", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("response") == "good" {
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
	}))
	defer srv.Close()
	c := New("s3cret").WithURL(srv.URL)
	ctx := context.Background()
	if ok, err := c.Verify(ctx, "good", "1.2.3.4"); !ok || err != nil {
		t.Errorf("good token: %v %v", ok, err)
	}
	if ok, err := c.Verify(ctx, "bad", ""); ok || err != nil {
		t.Errorf("bad token: %v %v", ok, err)
	}
	if ok, err := c.Verify(ctx, "", ""); ok || err != nil {
		t.Errorf("empty token: %v %v", ok, err)
	}
	if _, err := New("wrong").WithURL(srv.URL).Verify(ctx, "good", ""); !errors.Is(err, ErrUnavailable) {
		t.Errorf("server error should be ErrUnavailable, got %v", err)
	}
}
