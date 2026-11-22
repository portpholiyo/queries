package server

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipLimiter is a per-key token-bucket limiter with automatic cleanup.
type ipLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	rate    rate.Limit
	burst   int
}

type limiterEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// newIPLimiter allows one request per interval with the given burst.
func newIPLimiter(ctx context.Context, interval time.Duration, burst int) *ipLimiter {
	l := &ipLimiter{entries: make(map[string]*limiterEntry), rate: rate.Every(interval), burst: burst}
	go l.janitor(ctx)
	return l
}

// Allow reports whether key may proceed now.
func (l *ipLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &limiterEntry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	return e.lim.Allow()
}

func (l *ipLimiter) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.mu.Lock()
			for k, e := range l.entries {
				if now.Sub(e.lastSeen) > time.Hour {
					delete(l.entries, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
