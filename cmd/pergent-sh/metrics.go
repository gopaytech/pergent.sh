package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/metrics"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The labels that stand for what a metric must not name. A repository
// switched off, or a skill nobody lists any more, would otherwise stay in the
// page for as long as the database keeps its usage, and the number of series
// would only grow.
const (
	labelSwitchedOff = "(switched off)"
	labelOtherSkill  = "(other)"
	// labelOtherVerdict stands for a verdict this code does not know: the
	// result is the pergent job's own content, so its text is not a label.
	labelOtherVerdict = "(other)"
	// labelOtherStep stands for a failure before any Job existed (no step)
	// and for a step this code does not know.
	labelOtherStep = "other"
)

// metricsStore is the part of the store the metrics read.
type metricsStore interface {
	MetricsTotals(ctx context.Context) (store.MetricsTotals, error)
	LibrarySkills(ctx context.Context) ([]store.LibrarySkill, error)
}

// repoLister lists the repositories switched on, from the config and the
// dashboard: the list the usage page calls switched on.
type repoLister interface {
	List(ctx context.Context) ([]settings.Effective, error)
}

// metricsSource returns what a scrape reads: the totals from the database
// with the repository and skill names bounded to what is listed now.
func metricsSource(st metricsStore, repos repoLister) func(context.Context) (metrics.Snapshot, error) {
	return func(ctx context.Context) (metrics.Snapshot, error) {
		totals, err := st.MetricsTotals(ctx)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("read the totals: %w", err)
		}
		on, err := repos.List(ctx)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("list the repositories: %w", err)
		}
		library, err := st.LibrarySkills(ctx)
		if err != nil {
			return metrics.Snapshot{}, fmt.Errorf("list the skill library: %w", err)
		}
		return snapshotOf(totals, on, library), nil
	}
}

type kindCount struct {
	kind string
	n    int64
}

// tokenKinds are the token counts of rows run and skill rows, by kind. Cache
// and reasoning counts are unknown, not zero, for results from an older
// pergent: when no row reported them the series are left out. When some did,
// their sum stands.
func tokenKinds(t store.TokenSum, rows int64) []kindCount {
	kinds := []kindCount{{"input", t.In}, {"output", t.Out}}
	if t.Unknown < rows {
		kinds = append(kinds, kindCount{"reasoning", t.Reasoning}, kindCount{"cache_read", t.CacheRead}, kindCount{"cache_write", t.CacheWrite})
	}
	return kinds
}

// snapshotOf turns the totals into the metrics. A repository is named only if
// it is in on (matched ignoring case, as the platforms do, and spelled as on
// has it), and a skill only if the library or a switched-on repository's own
// lists name it, or it is the fixed impact check; everything else gets
// labelSwitchedOff or labelOtherSkill and is added to the others with the same
// labels. A verdict is one of the three pergent writes, or labelOtherVerdict.
// A step is one of the five a failed review records, or labelOtherStep.
// No merge request number, title or other text a person wrote is a label.
func snapshotOf(t store.MetricsTotals, on []settings.Effective, library []store.LibrarySkill) metrics.Snapshot {
	type repoKey struct{ instance, project string }
	repos := make(map[repoKey]string, len(on))
	skills := map[string]bool{result.ImpactSkill: true}
	for _, s := range library {
		skills[s.Name] = true
	}
	for _, e := range on {
		repos[repoKey{e.Instance, strings.ToLower(e.Project)}] = e.Project
		for _, name := range e.Skills {
			skills[name] = true
		}
		for _, name := range e.RepoSkills {
			skills[name] = true
		}
	}
	repo := func(instance, project string) string {
		if name, ok := repos[repoKey{instance, strings.ToLower(project)}]; ok {
			return name
		}
		return labelSwitchedOff
	}
	skill := func(name string) string {
		if skills[name] {
			return name
		}
		return labelOtherSkill
	}
	verdict := func(v string) string {
		switch v {
		case "no_blocking_issues", "has_blocking_issues", "failed":
			return v
		}
		return labelOtherVerdict
	}
	// result is what an API review ended in: the recorded outcome of a v1
	// verdict, or superseded.
	result := func(v string) string {
		switch v {
		case store.OutcomeNoBlocking, store.OutcomeBlocking, store.OutcomeNothingReviewed, store.OutcomeFailed, store.OutcomeSuperseded:
			return v
		}
		return labelOtherVerdict
	}
	callbackOutcome := func(v string) string {
		switch v {
		case store.CallbackOutcomeDelivered, store.CallbackOutcomeFailedAttempt, store.CallbackOutcomeGaveUp:
			return v
		}
		return labelOtherVerdict
	}
	step := func(v string) string {
		switch v {
		case kube.StepStarting, kube.StepClone, kube.StepReview, kube.StepDelivery, kube.StepUnknown:
			return v
		}
		return labelOtherStep
	}
	label := func(name, value string) metrics.Label { return metrics.Label{Name: name, Value: value} }

	snap := metrics.New()
	for _, u := range t.Tokens {
		base := []metrics.Label{label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)),
			label("skill", skill(u.Skill)), label("model", u.Model)}
		kinds := tokenKinds(u.Tokens, u.Rows)
		for _, k := range kinds {
			snap.Add(metrics.TokensTotal, k.n, append(base[:4:4], label("kind", k.kind))...)
		}
	}
	for _, u := range t.Reviews {
		snap.Add(metrics.ReviewsTotal, u.Reviews, label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)),
			label("trigger", u.Trigger), label("incremental", strconv.FormatBool(u.Incremental)))
	}
	for _, u := range t.NoResult {
		snap.Add(metrics.ReviewsNoResultTotal, u.Reviews, label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)),
			label("step", step(u.Step)))
	}
	for _, u := range t.Unfinished {
		snap.Add(metrics.ReviewsSupersededTotal, u.Superseded, label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)))
	}
	for _, u := range t.SkillRuns {
		snap.Add(metrics.SkillRunsTotal, u.Runs, label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)),
			label("skill", skill(u.Skill)), label("verdict", verdict(u.Verdict)))
	}
	for _, u := range t.Findings {
		base := []metrics.Label{label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)), label("skill", skill(u.Skill))}
		snap.Add(metrics.FindingsTotal, u.Blocking, append(base[:3:3], label("severity", "blocking"))...)
		snap.Add(metrics.FindingsTotal, u.Suggestions, append(base[:3:3], label("severity", "suggestion"))...)
	}
	for _, u := range t.Dismissals {
		snap.Add(metrics.FindingsDismissed, u.Dismissals, label("platform_instance", u.Instance), label("repository", repo(u.Instance, u.Project)),
			label("skill", skill(u.Skill)))
	}
	// An API client's name is its own label: clients are few and only an admin
	// makes one. No tenant, subject or key is a label.
	for _, u := range t.APIReviews {
		snap.Add(metrics.APIReviewsTotal, u.Reviews, label("client", u.Client), label("result", result(u.Result)))
	}
	for _, u := range t.APITokens {
		kinds := tokenKinds(u.Tokens, u.Rows)
		for _, k := range kinds {
			snap.Add(metrics.APITokensTotal, k.n, label("client", u.Client), label("kind", k.kind))
		}
	}
	for _, u := range t.APICallbacks {
		snap.Add(metrics.APICallbacksTotal, u.Count, label("client", u.Client), label("outcome", callbackOutcome(u.Outcome)))
	}
	return snap
}

// metricsHandler serves GET /metrics and nothing else: any other path is a
// 404, any other method on /metrics a 405. It shares no route with the main
// handler.
func metricsHandler(source func(context.Context) (metrics.Snapshot, error), log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler(source, log))
	return mux
}

// listenMetrics opens the metrics port before anything is served, so a port
// that is taken stops start-up. An empty addr means metrics are off.
func listenMetrics(addr string) (net.Listener, error) {
	if addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on metrics_listen %s: %w", addr, err)
	}
	return ln, nil
}

// serveMetrics returns a loop that serves handler on ln until ctx is done,
// then shuts down (20 s for requests in flight), as serveUntilDone does. A
// metrics server that fails is logged and ends its loop; it never takes the
// reviews down with it.
func serveMetrics(ln net.Listener, handler http.Handler, log *slog.Logger) func(context.Context) {
	return func(ctx context.Context) {
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
		served := make(chan error, 1)
		go func() { served <- srv.Serve(ln) }()
		select {
		case <-ctx.Done():
		case err := <-served:
			log.Error("the metrics server stopped", "error", err.Error())
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("metrics server didn't shut down cleanly", "error", err.Error())
		}
	}
}
