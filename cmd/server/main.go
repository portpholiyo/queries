// Command server runs the visitor-queries API and its Slack bridge.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/portpholiyo/queries/internal/captcha"
	"github.com/portpholiyo/queries/internal/config"
	"github.com/portpholiyo/queries/internal/server"
	"github.com/portpholiyo/queries/internal/slackx"
	"github.com/portpholiyo/queries/internal/store"
	"github.com/portpholiyo/queries/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, cfg.DBPassword)
	if err != nil {
		return err
	}
	defer st.Close()

	api := slack.New(cfg.SlackBotToken, slack.OptionAppLevelToken(cfg.SlackAppToken))
	sm := socketmode.New(api)
	w := worker.New(worker.Options{
		Store:         st,
		Slack:         slackx.New(api, cfg.SlackBotToken, cfg.SlackChannelID),
		OwnerID:       cfg.SlackOwnerID,
		MaxImages:     cfg.MaxImages,
		MaxImageBytes: cfg.MaxImageBytes,
	})

	var connected atomic.Bool
	srv := server.New(ctx, server.Options{
		Store:           st,
		Captcha:         captcha.New(cfg.CaptchaSecret),
		Notify:          w.Notify,
		IPSalt:          cfg.IPSalt,
		AllowedOrigins:  cfg.AllowedOrigins,
		ClientIPHeader:  cfg.ClientIPHeader,
		DailyCap:        cfg.DailyCap,
		MaxMessageChars: cfg.MaxMessageChars,
		MaxImages:       cfg.MaxImages,
		MaxImageBytes:   cfg.MaxImageBytes,
		SlackStatus:     connected.Load,
	})
	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second, // uploads of up to ~15 MB on slow links
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); w.Run(ctx) }()
	go func() { defer wg.Done(); runSocketMode(ctx, sm, cfg, w, &connected) }()
	go func() { defer wg.Done(); checkAuth(ctx, api) }()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		errCh <- httpSrv.ListenAndServe()
	}()

	var runErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
	}
	stop()
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil && runErr == nil {
		runErr = fmt.Errorf("shutdown: %w", err)
	}
	wg.Wait()
	return runErr
}

// checkAuth logs loudly if the Slack token is wrong, without stopping the
// service: visitors can still submit and queries wait in the queue.
func checkAuth(ctx context.Context, api *slack.Client) {
	for delay := 5 * time.Second; ctx.Err() == nil; delay = min(delay*2, 5*time.Minute) {
		res, err := api.AuthTestContext(ctx)
		if err == nil {
			slog.Info("slack authenticated", "team", res.Team, "bot", res.User)
			return
		}
		slog.Error("slack auth failed (queries will queue until fixed)", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
}

// runSocketMode receives Slack events over an outbound WebSocket (no public
// endpoint). The library reconnects automatically; a thread reply only
// triggers a re-read of that thread, so events carry no trusted data.
func runSocketMode(ctx context.Context, sm *socketmode.Client, cfg *config.Config, w *worker.Worker, connected *atomic.Bool) {
	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeConnected:
				connected.Store(true)
				slog.Info("slack socket connected")
			case socketmode.EventTypeDisconnect, socketmode.EventTypeConnectionError:
				connected.Store(false)
				slog.Warn("slack socket disconnected", "type", string(evt.Type))
			case socketmode.EventTypeEventsAPI:
				if evt.Request != nil {
					sm.Ack(*evt.Request)
				}
				ev, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok || ev.Type != slackevents.CallbackEvent {
					continue
				}
				m, ok := ev.InnerEvent.Data.(*slackevents.MessageEvent)
				if !ok || m.Channel != cfg.SlackChannelID || m.ThreadTimeStamp == "" || m.ThreadTimeStamp == m.TimeStamp {
					continue
				}
				w.EnqueueSync(m.ThreadTimeStamp)
			}
		}
	}()
	if err := sm.RunContext(ctx); err != nil && ctx.Err() == nil {
		slog.Error("slack socket stopped", "err", err)
	}
}

func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
