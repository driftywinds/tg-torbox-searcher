package torbox

import (
	"context"
	"sync"
	"time"
)

// simpleLimiter is a minimal token-bucket rate limiter (interval between
// tokens + a burst capacity). It exists purely so this project doesn't need
// golang.org/x/time/rate as a dependency (that module's vanity import
// resolution requires reaching golang.org, which isn't always reachable in
// restricted network environments). Behavior is equivalent to what we need
// here: "at most N events per interval, with a small burst allowance".
type simpleLimiter struct {
	mu       sync.Mutex
	interval time.Duration // time to accumulate one token
	burst    int
	tokens   float64
	last     time.Time
}

// newSimpleLimiter creates a limiter that allows one event every `interval`
// on average, allowing up to `burst` events to fire back-to-back before
// throttling kicks in.
func newSimpleLimiter(interval time.Duration, burst int) *simpleLimiter {
	if burst < 1 {
		burst = 1
	}
	return &simpleLimiter{
		interval: interval,
		burst:    burst,
		tokens:   float64(burst),
		last:     time.Now(),
	}
}

// Wait blocks until a token is available or ctx is done.
func (l *simpleLimiter) Wait(ctx context.Context) error {
	for {
		d, ok := l.tryTake()
		if ok {
			return nil
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *simpleLimiter) tryTake() (waitFor time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.last)
	l.last = now

	if l.interval > 0 {
		l.tokens += elapsed.Seconds() / l.interval.Seconds()
	}
	if l.tokens > float64(l.burst) {
		l.tokens = float64(l.burst)
	}

	if l.tokens >= 1 {
		l.tokens--
		return 0, true
	}

	missing := 1 - l.tokens
	wait := time.Duration(missing * float64(l.interval))
	if wait <= 0 {
		wait = time.Millisecond
	}
	return wait, false
}
