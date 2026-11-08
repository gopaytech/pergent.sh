// Package metrics writes pergent.sh's counters in the Prometheus text
// exposition format, version 0.0.4, by hand: the format is small and the
// service has no other use for a client library. Every value is computed
// from the database at each scrape, so the counters survive restarts.
package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The metric names. All are counters.
const (
	TokensTotal            = "pergent_tokens_total"
	ReviewsTotal           = "pergent_reviews_total"
	ReviewsNoResultTotal   = "pergent_reviews_no_result_total"
	ReviewsSupersededTotal = "pergent_reviews_superseded_total"
	SkillRunsTotal         = "pergent_skill_runs_total"
	FindingsTotal          = "pergent_findings_total"
	DismissalsTotal        = "pergent_dismissals_total"
)

// names lists the metrics New declares, with their help text.
var names = []string{TokensTotal, ReviewsTotal, ReviewsNoResultTotal, ReviewsSupersededTotal,
	SkillRunsTotal, FindingsTotal, DismissalsTotal}

var help = map[string]string{
	TokensTotal:            "Tokens used by reviews, by kind: input, output, reasoning, cache_read or cache_write. The reasoning, cache_read and cache_write series are absent for a repository, skill and model when no review reported them, and count only the reviews that did when some did not.",
	ReviewsTotal:           "Reviews that left a result, by what started them and whether they covered only the files changed since the last review.",
	ReviewsNoResultTotal:   "Reviews that failed before leaving a result.",
	ReviewsSupersededTotal: "Reviews stopped by a newer push before they left a result.",
	SkillRunsTotal:         "Skill runs, by verdict.",
	FindingsTotal:          "Findings reported, by severity: blocking or suggestion.",
	DismissalsTotal:        "Findings people dismissed. Dismissals are deleted when a repository is switched off, so this counter can drop, which Prometheus reads as a reset.",
}

// Label is one label of a series.
type Label struct{ Name, Value string }

// Sample is the value of one series. Samples of a metric with the same label
// values are added when written, so a series is never listed twice.
type Sample struct {
	Labels []Label
	Value  int64
}

// Metric is a counter and its series.
type Metric struct {
	Name, Help string
	Samples    []Sample
}

// Snapshot is the counters at one moment.
type Snapshot struct {
	Metrics []Metric
}

// New returns a snapshot that declares every metric, with no series.
func New() Snapshot {
	s := Snapshot{Metrics: make([]Metric, 0, len(names))}
	for _, n := range names {
		s.Metrics = append(s.Metrics, Metric{Name: n, Help: help[n]})
	}
	return s
}

// Add adds value to the series of the named metric with these labels. The
// metric must be one New declares.
func (s *Snapshot) Add(name string, value int64, labels ...Label) {
	for i := range s.Metrics {
		if s.Metrics[i].Name == name {
			s.Metrics[i].Samples = append(s.Metrics[i].Samples, Sample{Labels: labels, Value: value})
			return
		}
	}
	panic("metrics: unknown metric " + name)
}

// escapeLabel escapes a label value as the text format asks: backslash, double
// quote and line feed. A byte that is not UTF-8 would make the page invalid,
// so it becomes the replacement character.
func escapeLabel(v string) string {
	return labelEscaper.Replace(strings.ToValidUTF8(v, "�"))
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeHelp escapes help text: backslash and line feed.
var escapeHelp = strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace

// seriesKey is the sort key of a sample: its label values, then names.
func seriesKey(labels []Label) string {
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(l.Value)
		b.WriteByte(0)
	}
	for _, l := range labels {
		b.WriteString(l.Name)
		b.WriteByte(0)
	}
	return b.String()
}

// Write prints s in the text format: each metric once with its # HELP and
// # TYPE lines, metrics and series in a fixed order, series with the same
// labels added.
func Write(w io.Writer, s Snapshot) error {
	var buf bytes.Buffer
	metrics := slices.Clone(s.Metrics)
	slices.SortStableFunc(metrics, func(a, b Metric) int { return strings.Compare(a.Name, b.Name) })
	for _, m := range metrics {
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s counter\n", m.Name, escapeHelp(m.Help), m.Name)
		type keyed struct {
			key string
			Sample
		}
		series := make([]keyed, len(m.Samples))
		for i, smp := range m.Samples {
			series[i] = keyed{seriesKey(smp.Labels), smp}
		}
		slices.SortStableFunc(series, func(a, b keyed) int { return strings.Compare(a.key, b.key) })
		for i := 0; i < len(series); {
			sum := series[i].Value
			j := i + 1
			for ; j < len(series) && series[j].key == series[i].key; j++ {
				sum += series[j].Value
			}
			buf.WriteString(m.Name)
			if labels := series[i].Labels; len(labels) > 0 {
				buf.WriteByte('{')
				for k, l := range labels {
					if k > 0 {
						buf.WriteByte(',')
					}
					fmt.Fprintf(&buf, `%s="%s"`, l.Name, escapeLabel(l.Value))
				}
				buf.WriteByte('}')
			}
			fmt.Fprintf(&buf, " %d\n", sum)
			i = j
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// queryTimeout bounds one scrape's reads from the database.
const queryTimeout = 10 * time.Second

// Handler answers a scrape with the snapshot source returns. A source that
// fails gives a 500 with an empty body: the error is logged, never sent, as
// it may name a query or a table.
func Handler(source func(context.Context) (Snapshot, error), log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		snap, err := source(ctx)
		if err != nil {
			log.Error("reading metrics failed", "error", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var buf bytes.Buffer
		if err := Write(&buf, snap); err != nil {
			log.Error("writing metrics failed", "error", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	})
}
