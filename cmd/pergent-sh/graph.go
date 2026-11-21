package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopaytech/pergent/graph"

	"github.com/gopaytech/pergent.sh/internal/codegraph"
)

// post-graph runs in the "post" container of a graph build Job
// (kube.BuildGraphJob), after the clone wrote the commit to <work>/commit (or
// its failure to <work>/error) and graphify and pergent's converter wrote
// <out>/graph.json (or their failure to <out>/error). It posts the graph file
// as it is, or the first error, to --result-url with the build token
// (PERGENT_SH_GRAPH_TOKEN) as bearer. pergent.sh checks the graph; this
// command only reads files and holds no other secret.
//
// get-graph runs in the "graph" init container of a review Job
// (kube.BuildJob with a graph URL). It downloads the run's graph from --url
// with the run's result token (PERGENT_RESULT_TOKEN) to --out, written whole
// or not at all. It always exits 0: a review with no graph is a review as
// before.
//
// get-panel runs in the "panel-inputs" init container of a review panel's
// merge Job (kube.BuildJob with a panel). It downloads member A's and member
// B's results from --a-url and --b-url to <dir>/a.json and <dir>/b.json, with
// the same code and token as get-graph. A member with no result (404), or one
// that still fails after 3 tries, gets no file. It always exits 0: pergent
// merges what it gets, and says so.

type postGraphOptions struct{ work, out, resultURL string }

type getGraphOptions struct{ url, out string }

type getPanelOptions struct{ aURL, bURL, dir string }

var (
	// maxGraphPost and maxGraphGet are graph.MaxBytes, as variables so the
	// tests can lower them.
	maxGraphPost int64 = graph.MaxBytes
	maxGraphGet  int64 = graph.MaxBytes
	// maxPanelGet bounds one member result (10 MiB), as a variable for the
	// tests.
	maxPanelGet int64 = 10 << 20
	// panelRetryWait is the wait before the second try; it doubles before
	// the third. A variable so the tests can drop it.
	panelRetryWait = time.Second
)

const (
	// getGraphTimeout bounds the whole download; getPanelTimeout bounds
	// get-panel's, both results and every try.
	getGraphTimeout = 2 * time.Minute
	getPanelTimeout = 2 * time.Minute
	// panelTries is how many times get-panel asks for one result.
	panelTries = 3
)

func parsePostGraphArgs(args []string, stderr io.Writer) (postGraphOptions, error) {
	flags := flag.NewFlagSet("post-graph", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o postGraphOptions
	flags.StringVar(&o.work, "work", "", "the clone's folder: repo, commit and error")
	flags.StringVar(&o.out, "out", "", "graphify's folder: graph.json and error")
	flags.StringVar(&o.resultURL, "result-url", "", "where to post the graph")
	if err := flags.Parse(args); err != nil {
		return postGraphOptions{}, err
	}
	switch {
	case flags.NArg() > 0:
		return postGraphOptions{}, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	case o.work == "" || o.out == "" || o.resultURL == "":
		return postGraphOptions{}, errors.New("post-graph needs --work, --out and --result-url\n" + usage)
	}
	if err := checkResultURL(o.resultURL); err != nil {
		return postGraphOptions{}, err
	}
	return o, nil
}

func parseGetGraphArgs(args []string, stderr io.Writer) (getGraphOptions, error) {
	flags := flag.NewFlagSet("get-graph", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o getGraphOptions
	flags.StringVar(&o.url, "url", "", "the run's graph URL")
	flags.StringVar(&o.out, "out", "", "the file to write")
	if err := flags.Parse(args); err != nil {
		return getGraphOptions{}, err
	}
	switch {
	case flags.NArg() > 0:
		return getGraphOptions{}, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	case o.url == "" || o.out == "":
		return getGraphOptions{}, errors.New("get-graph needs --url and --out\n" + usage)
	}
	if !httpURL(o.url) {
		return getGraphOptions{}, errors.New("--url must be an http or https URL")
	}
	return o, nil
}

func parseGetPanelArgs(args []string, stderr io.Writer) (getPanelOptions, error) {
	flags := flag.NewFlagSet("get-panel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o getPanelOptions
	flags.StringVar(&o.aURL, "a-url", "", "member A's result URL")
	flags.StringVar(&o.bURL, "b-url", "", "member B's result URL")
	flags.StringVar(&o.dir, "dir", "", "the folder to write a.json and b.json in")
	if err := flags.Parse(args); err != nil {
		return getPanelOptions{}, err
	}
	switch {
	case flags.NArg() > 0:
		return getPanelOptions{}, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	case o.aURL == "" || o.bURL == "" || o.dir == "":
		return getPanelOptions{}, errors.New("get-panel needs --a-url, --b-url and --dir\n" + usage)
	case !httpURL(o.aURL) || !httpURL(o.bURL):
		return getPanelOptions{}, errors.New("--a-url and --b-url must be http or https URLs")
	}
	return o, nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// runPostGraph is the post-graph command. It exits 0 when pergent.sh took
// what it posted and 1 when not.
func runPostGraph(ctx context.Context, o postGraphOptions, stderr io.Writer) int {
	token := os.Getenv("PERGENT_SH_GRAPH_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "environment variable PERGENT_SH_GRAPH_TOKEN is not set or empty")
		return 1
	}
	if err := postGraph(ctx, o, token, newResultClient(), stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// postGraph posts the graph, or the error that stands in for it.
func postGraph(ctx context.Context, o postGraphOptions, token string, client *http.Client, stderr io.Writer) error {
	body, failure := graphBody(o, stderr)
	if failure != "" {
		b, err := json.Marshal(codegraph.ErrorBody{Error: failure})
		if err != nil {
			return fmt.Errorf("encode the error: %w", err)
		}
		body = b
	}
	if err := postResult(ctx, client, o.resultURL, token, body, stderr); err != nil {
		return err
	}
	if failure != "" {
		fmt.Fprintln(stderr, "sent the error:", failure)
	} else {
		fmt.Fprintf(stderr, "sent the graph (%d bytes)\n", len(body))
	}
	return nil
}

// graphBody is the graph file, or the failure to send: the clone's error, a
// clone that wrote no commit, graphify's error, no graph, a graph over the
// limit.
func graphBody(o postGraphOptions, stderr io.Writer) ([]byte, string) {
	firstLine := func(file, fallback string) (string, bool) {
		text, ok, err := readSmall(file, maxErrorFile)
		if err != nil {
			fmt.Fprintln(stderr, "reading", file+":", err)
			return fallback, true
		}
		if !ok {
			return "", false
		}
		text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
		if text == "" {
			text = fallback
		}
		return text, true
	}
	if msg, ok := firstLine(filepath.Join(o.work, "error"), codegraph.MsgCloneUnknown); ok {
		return nil, msg
	}
	if commit, ok, err := readSmall(filepath.Join(o.work, "commit"), maxCommitFile); err != nil || !ok || strings.TrimSpace(commit) == "" {
		return nil, codegraph.MsgCloneUnknown
	}
	if msg, ok := firstLine(filepath.Join(o.out, "error"), codegraph.MsgNoGraph); ok {
		return nil, msg
	}
	f, err := os.Open(filepath.Join(o.out, "graph.json"))
	if err != nil {
		fmt.Fprintln(stderr, "opening the graph:", err)
		return nil, codegraph.MsgNoGraph
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxGraphPost+1))
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "reading the graph:", err)
		return nil, codegraph.MsgNoGraph
	case int64(len(b)) > maxGraphPost:
		return nil, codegraph.MsgTooLarge
	}
	return b, ""
}

// runGetGraph is the get-graph command. It always exits 0.
func runGetGraph(ctx context.Context, o getGraphOptions, stderr io.Writer) int {
	token := os.Getenv("PERGENT_RESULT_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "graph: not downloaded: PERGENT_RESULT_TOKEN is not set")
		return 0
	}
	getGraph(ctx, o, token, newResultClient(), stderr)
	return 0
}

// getGraph downloads the graph to o.out through a temp file beside it, so the
// file is whole or not there. Every failure is one log line.
func getGraph(ctx context.Context, o getGraphOptions, token string, client *http.Client, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, getGraphTimeout)
	defer cancel()
	d := download{url: o.url, out: o.out, token: token, max: maxGraphGet, mode: 0o644,
		notFound: "no graph for this review", tooLarge: "the graph is over the size limit"}
	if err := d.get(ctx, client); err != nil {
		fmt.Fprintln(stderr, "graph: not downloaded:", err)
		return
	}
	fmt.Fprintln(stderr, "graph: downloaded")
}

// runGetPanel is the get-panel command. It always exits 0.
func runGetPanel(ctx context.Context, o getPanelOptions, stderr io.Writer) int {
	token := os.Getenv("PERGENT_RESULT_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "panel: not downloaded: PERGENT_RESULT_TOKEN is not set")
		return 0
	}
	getPanel(ctx, o, token, newResultClient(), stderr)
	return 0
}

// getPanel downloads each member's result, mode 0600, whole or not at all.
// A failure other than a 404 or a result over the limit is tried again, up to
// panelTries times. Every outcome is one log line per member.
func getPanel(ctx context.Context, o getPanelOptions, token string, client *http.Client, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, getPanelTimeout)
	defer cancel()
	for _, m := range []struct{ role, url string }{{"a", o.aURL}, {"b", o.bURL}} {
		d := download{url: m.url, out: filepath.Join(o.dir, m.role+".json"), token: token, max: maxPanelGet, mode: 0o600,
			notFound: "no result for this member", tooLarge: "the result is over the size limit"}
		if err := d.getWithRetry(ctx, client, panelTries, panelRetryWait); err != nil {
			fmt.Fprintf(stderr, "panel %s: not downloaded: %v\n", m.role, err)
			continue
		}
		fmt.Fprintf(stderr, "panel %s: downloaded\n", m.role)
	}
}

// download is one file a Job's init container fetches from pergent.sh with
// the run's result token: get-graph's and get-panel's shared code.
type download struct {
	url, out, token    string
	max                int64
	mode               os.FileMode
	notFound, tooLarge string // the messages of a 404 and of a file over max
}

// finalError is a failure that another try would not change.
type finalError struct{ msg string }

func (e finalError) Error() string { return e.msg }

// getWithRetry is get, tried up to tries times while the failure is not
// final, waiting wait, then twice that, between tries.
func (d download) getWithRetry(ctx context.Context, client *http.Client, tries int, wait time.Duration) error {
	var err error
	for i := 0; i < tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(wait):
			}
			wait *= 2
		}
		if err = d.get(ctx, client); err == nil || errors.As(err, new(finalError)) {
			return err
		}
	}
	return err
}

// get writes the file to d.out through a temp file beside it, so the file is
// whole or not there.
func (d download) get(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return finalError{"bad URL"}
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("pergent.sh can't be reached")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return finalError{d.notFound}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("pergent.sh answered %s", resp.Status)
	}
	tmp := d.out + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, d.mode)
	if err != nil {
		return fmt.Errorf("create the file: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, d.max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		_ = os.Remove(tmp)
		return errors.New("the download broke off")
	case n > d.max:
		_ = os.Remove(tmp)
		return finalError{d.tooLarge}
	}
	if err := os.Rename(tmp, d.out); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write the file: %w", err)
	}
	return nil
}
