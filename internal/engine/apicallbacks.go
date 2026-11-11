package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// callbackInterval is how often RunCallbacks looks for callbacks due,
	// callbackSenders how many it sends at once and callbackBatch how many
	// it takes in one pass.
	callbackInterval = 2 * time.Second
	callbackSenders  = 4
	callbackBatch    = 50
	// callbackTimeout is how long a callback URL has to answer.
	callbackTimeout = 10 * time.Second
	// A failed callback is tried again after callbackFirstRetry, doubling up
	// to callbackMaxRetry, until callbackWindow after the review ended.
	callbackFirstRetry = 10 * time.Second
	callbackMaxRetry   = time.Hour
	callbackWindow     = 24 * time.Hour
	// callbackFailingThreshold is how many callbacks of a client made in the
	// last callbackWindow, not delivered (CountUndeliveredAPICallbacks), raise
	// callback_failing.
	callbackFailingThreshold = 3
	// maxCallbackError is the most of an attempt's error that is stored.
	maxCallbackError = 500
	// callbackDrain is the most of a callback URL's answer that is read.
	callbackDrain = 64 << 10

	msgCallbackRedirect   = "the callback URL answered with a redirect, which is not followed"
	msgCallbackSecretGone = "the client's callback secret can't be opened: rotate it"
	callbackHidden        = "***"
)

// RunCallbacks sends the callbacks of API reviews until ctx is done: every
// 2 s, the ones due (DeliverCallbacksOnce). It is a loop of its own, so a
// slow callback URL never holds up a review. Errors are logged and never stop
// it.
func (e *Engine) RunCallbacks(ctx context.Context) {
	tick := time.NewTicker(callbackInterval)
	defer tick.Stop()
	for {
		if err := e.DeliverCallbacksOnce(ctx); err != nil && ctx.Err() == nil {
			e.log().Error("callbacks failed", "error", e.redact(err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DeliverCallbacksOnce sends the callbacks due now, at most 4 at once, and
// records each attempt. Then, for each client it tried, callback_failing is
// set or cleared from its count. The returned error joins the failures to
// read or record; a callback URL that fails is an attempt, not an error.
func (e *Engine) DeliverCallbacksOnce(ctx context.Context) error {
	due, err := e.Store.DueAPICallbacks(ctx, e.now(), callbackBatch)
	if err != nil {
		return fmt.Errorf("list the callbacks due: %w", err)
	}
	if len(due) == 0 {
		return nil
	}
	hc := e.callbackClient()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		tried = map[int64]string{}
		slots = make(chan struct{}, callbackSenders)
	)
	for _, cb := range due {
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			ok, err := e.sendCallback(ctx, hc, cb)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				tried[cb.ClientID] = cb.ClientName
			}
		}()
	}
	wg.Wait()
	// After the pass, not in each sender, so two attempts of one client
	// never race to set and clear its item.
	for id, name := range tried {
		e.refreshCallbackFailing(ctx, id, name)
	}
	return errors.Join(errs...)
}

// callbackClient is CallbackClient, or one with a 10 s timeout, made never
// to follow a redirect: a callback goes to the URL an admin typed, and
// nowhere its answer points.
func (e *Engine) callbackClient() *http.Client {
	hc := http.Client{Timeout: callbackTimeout}
	if e.CallbackClient != nil {
		hc = *e.CallbackClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

// sendCallback makes one attempt of cb and records it. The URL and secrets
// are cb's, read just now; the body is the review's GET body now. tried is
// false when nothing was attempted: the review is gone with its client, or
// ctx ended during the send (the next start tries again, under the same
// delivery ID).
func (e *Engine) sendCallback(ctx context.Context, hc *http.Client, cb store.APICallback) (tried bool, err error) {
	log := e.log().With("callback", cb.ID, "run", cb.RunID, "api_client", cb.ClientName)
	review, found, err := e.Store.APIReviewByID(ctx, cb.RunID)
	if err != nil {
		return false, fmt.Errorf("load the review of callback %s: %w", cb.ID, err)
	}
	if !found {
		// Removed with its client since it was listed: its callback is gone.
		return false, nil
	}
	now := e.now()
	a := store.CallbackAttempt{At: now}
	if cb.URL == "" {
		a.Error = store.CallbackNoURL
		log.Warn("giving up on a callback: the client has no callback URL")
		return true, e.recordCallback(ctx, cb, a, false, true)
	}
	body, err := reviewapi.Body(review)
	if err != nil {
		return false, fmt.Errorf("encode the review of callback %s: %w", cb.ID, err)
	}

	secrets, ok := e.callbackSecrets(cb, now, log)
	if !ok {
		a.Error = msgCallbackSecretGone
	} else {
		a.StatusCode, a.Error, a.Duration = e.postCallback(ctx, hc, cb, body, now, secrets)
		if ctx.Err() != nil {
			return false, nil
		}
	}
	delivered := a.Error == "" && a.StatusCode >= 200 && a.StatusCode < 300
	gaveUp := !delivered && !now.Before(cb.CreatedAt.Add(callbackWindow))
	switch {
	case delivered:
		log.Info("callback delivered", "status", a.StatusCode, "attempt", cb.Attempts+1)
	case gaveUp:
		log.Error("giving up on a callback after 24 hours", "status", a.StatusCode, "error", a.Error, "attempts", cb.Attempts+1)
	default:
		log.Warn("callback failed; will try again", "status", a.StatusCode, "error", a.Error, "attempt", cb.Attempts+1,
			"retry_at", now.Add(callbackBackoff(cb.Attempts)))
	}
	return true, e.recordCallback(ctx, cb, a, delivered, gaveUp)
}

// callbackSecrets opens the client's secret and, while the rotation's
// overlap lasts, the previous one after it. ok is false when the current one
// can't be opened; a previous one that can't is left out.
func (e *Engine) callbackSecrets(cb store.APICallback, now time.Time, log *slog.Logger) ([]string, bool) {
	cur, err := e.Box.Open(cb.SecretSealed)
	if err != nil {
		log.Warn("the client's callback secret can't be opened")
		return nil, false
	}
	secrets := []string{string(cur)}
	if len(cb.PreviousSealed) > 0 && cb.PreviousUntil.After(now) {
		prev, err := e.Box.Open(cb.PreviousSealed)
		if err != nil {
			log.Warn("the client's previous callback secret can't be opened; signing with the current one alone")
		} else {
			secrets = append(secrets, string(prev))
		}
	}
	return secrets, true
}

// postCallback posts body, signed with secrets at now, and returns the
// status (0 for no answer), the error to store (redacted, "" when the URL
// answered without a redirect) and how long it took.
func (e *Engine) postCallback(ctx context.Context, hc *http.Client, cb store.APICallback, body []byte, now time.Time, secrets []string) (int, string, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cb.URL, bytes.NewReader(body))
	if err != nil {
		return 0, callbackError(e.redact(err.Error()), cb.URL, secrets), 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pergent-Delivery", cb.ID)
	req.Header.Set("X-Pergent-Signature", reviewapi.Sign(body, now, secrets...))
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, callbackError(e.redact(err.Error()), cb.URL, secrets), time.Since(start)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, callbackDrain))
	_ = resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, msgCallbackRedirect, took
	}
	return resp.StatusCode, "", took
}

// recordCallback records attempt a of cb, within a settle context of its
// own, so an attempt made just before a stop is still recorded.
func (e *Engine) recordCallback(ctx context.Context, cb store.APICallback, a store.CallbackAttempt, delivered, gaveUp bool) error {
	sctx, cancel := settleCtx(ctx)
	defer cancel()
	if err := e.Store.RecordAPICallbackAttempt(sctx, cb.ID, a, delivered, a.At.Add(callbackBackoff(cb.Attempts)), gaveUp); err != nil {
		return fmt.Errorf("record an attempt of callback %s: %w", cb.ID, err)
	}
	return nil
}

// callbackBackoff is the wait before the next try of a callback that failed
// attempts times before this failure: 10 s × 2^attempts, at most an hour.
func callbackBackoff(attempts int) time.Duration {
	// 10 s × 2^9 is already over the hour; the cap also keeps the shift small.
	if attempts >= 9 {
		return callbackMaxRetry
	}
	return min(callbackFirstRetry<<attempts, callbackMaxRetry)
}

// callbackError is text as an attempt stores it, and logs it: the URL with
// its path, query and fragment replaced by *** where it is quoted whole (as
// net/http quotes it), the query, fragment, user info and secrets replaced
// wherever else they are, and cut to 500 bytes on a rune boundary. A webhook
// URL often holds a token in its path or query.
func callbackError(text, rawURL string, secrets []string) string {
	hide := append([]string(nil), secrets...)
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		hide = append(hide, rawURL[i+1:])
	}
	if u, err := url.Parse(rawURL); err == nil {
		if u.Host != "" {
			shown := shownURL(u)
			text = strings.ReplaceAll(text, rawURL, shown)
			text = strings.ReplaceAll(text, u.String(), shown)
		}
		hide = append(hide, u.RawQuery, u.EscapedFragment(), u.Fragment)
		if u.User != nil {
			hide = append(hide, u.User.String())
		}
	}
	text = replaceAll(text, callbackHidden, hide...)
	if len(text) > maxCallbackError {
		text = text[:maxCallbackError]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

// shownURL is u as an error may show it: its scheme and host, and *** for a
// path other than "/", a query and a fragment.
func shownURL(u *url.URL) string {
	s := u.Scheme + "://" + u.Host
	switch u.Path {
	case "", "/":
		s += u.Path
	default:
		s += "/" + callbackHidden
	}
	if u.RawQuery != "" || u.ForceQuery {
		s += "?" + callbackHidden
	}
	if u.Fragment != "" {
		s += "#" + callbackHidden
	}
	return s
}

// replaceAll replaces every one of values in text by with, longest first, so
// a value holding another is replaced whole, as redactAll does. Empty values
// are skipped.
func replaceAll(text, with string, values ...string) string {
	var list []string
	for _, v := range values {
		if v != "" {
			list = append(list, v)
		}
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, v := range list {
		text = strings.ReplaceAll(text, v, with)
	}
	return text
}

// refreshCallbackFailing sets callback_failing for the client when at least
// three of its callbacks made in the last 24 hours are undelivered, and
// clears it when fewer are. A count that can't be read leaves the item as it
// was.
func (e *Engine) refreshCallbackFailing(ctx context.Context, clientID int64, name string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attentionTimeout)
	defer cancel()
	n, err := e.Store.CountUndeliveredAPICallbacks(sctx, clientID, e.now().Add(-callbackWindow))
	if err != nil {
		e.log().Info("couldn't count the client's undelivered callbacks", "api_client", name, "error", e.redact(err.Error()))
		return
	}
	if n < callbackFailingThreshold {
		e.clearAttention(sctx, "", name, attCallbackFailing)
		return
	}
	e.setAttention(sctx, "", name, attCallbackFailing, fmt.Sprintf("Callbacks to this client failed for %d reviews in the last 24 hours. "+
		"Check its callback URL; the results stay readable with GET /v1/reviews/{id}.", n))
}
