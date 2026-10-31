package dashboard

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// limitAttempts password attempts (sign-ins and changes) are allowed
	// per client IP in any limitWindow.
	limitAttempts = 20
	limitWindow   = 5 * time.Minute
	// limitMaxIPs is the most client IPs the limiter holds at once. Past
	// it, an IP it doesn't hold is refused (429) until the old ones leave
	// the window, so the map can't grow without bound.
	limitMaxIPs = 100000
)

// limiter counts password attempts by client IP, in memory (the dashboard
// runs one replica).
type limiter struct {
	mu        sync.Mutex
	hits      map[string][]time.Time // attempts in the window, oldest first
	lastSweep time.Time
	maxIPs    int
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}, maxIPs: limitMaxIPs} }

// allow records an attempt from ip at now and reports whether it is within
// the limit. A refused attempt is not counted. Attempts older than the
// window are dropped as it goes: those of ip on every call, everyone's once
// per window. An ip it doesn't hold is refused while it holds maxIPs.
func (l *limiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) >= limitWindow {
		for k, hits := range l.hits {
			if hits = recent(hits, now); len(hits) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = hits
			}
		}
		l.lastSweep = now
	}
	hits, known := l.hits[ip]
	if !known && len(l.hits) >= l.maxIPs {
		return false
	}
	hits = recent(hits, now)
	if len(hits) >= limitAttempts {
		l.hits[ip] = hits
		return false
	}
	l.hits[ip] = append(hits, now)
	return true
}

// recent is hits without those older than the window at now.
func recent(hits []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(hits) && now.Sub(hits[i]) >= limitWindow {
		i++
	}
	return hits[i:]
}

// clientIP is the request's client: the last X-Forwarded-For entry (the one
// the proxy in front of pergent.sh added) when there is one, else the host
// of RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		entries := strings.Split(xff[len(xff)-1], ",")
		if last := strings.TrimSpace(entries[len(entries)-1]); last != "" {
			return last
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
