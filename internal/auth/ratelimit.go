package auth

import (
	"sync"
	"time"
)

const (
	loginFailureWindow = 15 * time.Minute
	loginFailureLimit  = 5
)

type limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{failures: make(map[string][]time.Time)}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)
	return len(l.failures[ip]) < loginFailureLimit
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanup(now)
	l.failures[ip] = append(l.failures[ip], now)
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

func (l *limiter) cleanup(now time.Time) {
	cutoff := now.Add(-loginFailureWindow)
	for ip, failures := range l.failures {
		kept := failures[:0]
		for _, failure := range failures {
			if failure.After(cutoff) {
				kept = append(kept, failure)
			}
		}
		if len(kept) == 0 {
			delete(l.failures, ip)
		} else {
			l.failures[ip] = kept
		}
	}
}
