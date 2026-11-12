package reviewapi

import (
	"strconv"
	"sync"
	"time"
)

// Limiter holds each key to its client's requests per hour: a token bucket
// per key, in memory, that holds perHour requests and refills at perHour an
// hour. A bucket is kept as the time it is full again, so the sums are whole
// durations. A restart empties it (there is one replica), which gives every
// key a full bucket. The zero value is ready to use.
type Limiter struct {
	mu   sync.Mutex
	full map[int64]time.Time
}

// Allow takes one request from the bucket of keyID at now. When the bucket is
// empty nothing is taken: ok is false and retryAfter is how long until it
// holds one again. A perHour below 1 counts as 1.
func (l *Limiter) Allow(keyID int64, perHour int, now time.Time) (ok bool, retryAfter time.Duration) {
	perHour = max(perHour, 1)
	// Each request fills one interval of the hour; the bucket is empty when
	// the hour is all filled.
	interval := time.Hour / time.Duration(perHour)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.full == nil {
		l.full = map[int64]time.Time{}
	}
	full := l.full[keyID]
	if full.Before(now) {
		full = now
	}
	if wait := full.Add(interval).Sub(now) - time.Hour; wait > 0 {
		return false, wait
	}
	l.full[keyID] = full.Add(interval)
	return true, 0
}

// retryAfter is d as a Retry-After value: whole seconds, rounded up, at least 1.
func retryAfter(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(max(secs, 1), 10)
}
