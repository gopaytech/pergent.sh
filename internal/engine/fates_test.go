package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
	"github.com/gopaytech/pergent.sh/internal/fates"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// ---------------------------------------------------------------------------
// fatesInput: no database

// fateFinding is a finding of severity at line of db/query.go.
func fateFinding(fingerprint, severity string, line int) result.Finding {
	return result.Finding{ID: "id-" + fingerprint, Fingerprint: fingerprint, File: "db/query.go", Line: line, Severity: severity,
		Title: "a title", Explanation: "an explanation"}
}

// fateSkill is the "security" skill with verdict that reviewed db/query.go
// and reported findings.
func fateSkill(t *testing.T, verdict string, findings ...result.Finding) result.Skill {
	t.Helper()
	s := reviewed(verdict, "db/query.go")
	for _, f := range findings {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		s.Findings = append(s.Findings, b)
	}
	return s
}

func TestFatesInputParsesReviews(t *testing.T) {
	first := store.Run{HeadSHA: sha('a'), SkillHashes: map[string]string{"security": "h1"},
		Result: resultFor(t, sha('a'), fateSkill(t, "has_blocking_issues", fateFinding("aaaaaaaaaaaa", "blocking", 12), fateFinding("bbbbbbbbbbbb", "suggestion", 0)))}
	// A skill that failed, and a finding with no fingerprint (dropped).
	second := store.Run{HeadSHA: sha('b'),
		Result: resultFor(t, sha('b'), fateSkill(t, "failed", fateFinding("", "blocking", 3)))}
	ds := []store.Dismissal{{Fingerprint: "aaaaaaaaaaaa", Severity: "blocking", Reason: "no", Actor: "maya"}}

	got := fatesInput([]store.Run{first, second}, ds)

	want := fates.Input{
		Reviews: []fates.Review{
			{Head: sha('a'), Skills: []fates.Skill{{
				Name: "security", Hash: "h1", Reviewed: map[string]bool{"db/query.go": true},
				Findings: []fates.Finding{
					{Fingerprint: "aaaaaaaaaaaa", File: "db/query.go", Line: 12, Severity: "blocking"},
					{Fingerprint: "bbbbbbbbbbbb", File: "db/query.go", Line: 0, Severity: "suggestion"},
				},
			}}},
			{Head: sha('b'), Skills: []fates.Skill{{
				Name: "security", Failed: true, Reviewed: map[string]bool{"db/query.go": true},
			}}},
		},
		Dismissals: []fates.Dismissal{{Fingerprint: "aaaaaaaaaaaa", Severity: "blocking"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fatesInput =\n%+v\nwant\n%+v", got, want)
	}
}

func TestFatesInputSkipsUnreadableResults(t *testing.T) {
	good := resultFor(t, sha('a'), fateSkill(t, "no_blocking_issues"))
	errResult, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "error", Head: sha('b'),
		Error:   &result.Error{Code: "internal", Message: "boom"},
		Comment: result.Comment{Markdown: "the review failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(good), `"schema_version":1`, `"schema_version":2`, 1)
	if newer == string(good) {
		t.Fatal("the test result has no schema_version to change")
	}

	runs := []store.Run{
		{HeadSHA: sha('1'), Result: nil},
		{HeadSHA: sha('2'), Result: []byte("not json")},
		{HeadSHA: sha('3'), Result: errResult},
		{HeadSHA: sha('4'), Result: []byte(newer)},
		{HeadSHA: sha('a'), Result: good},
	}
	got := fatesInput(runs, nil)
	if len(got.Reviews) != 1 || got.Reviews[0].Head != sha('a') {
		t.Errorf("reviews = %+v; want only the readable completed one", got.Reviews)
	}
}

func TestFatesInputLeftOutNote(t *testing.T) {
	left := fateSkill(t, "has_blocking_issues")
	left.Notes = []string{"Something else.", result.LeftOutNotePrefix + " Token compared with == (in api/auth.go)"}
	other := fateSkill(t, "has_blocking_issues")
	other.Name = "style"
	other.Notes = []string{"Left out some other way."}
	got := fatesInput([]store.Run{{HeadSHA: sha('a'), Result: resultFor(t, sha('a'), left, other)}}, nil)

	skills := got.Reviews[0].Skills
	if !skills[0].LeftOut || skills[1].LeftOut {
		t.Errorf("LeftOut = %v, %v; want true, false", skills[0].LeftOut, skills[1].LeftOut)
	}
}

func TestFatesInputCarriedFilesAreReviewed(t *testing.T) {
	s := fateSkill(t, "no_blocking_issues")
	s.Files.Carried = []string{"api/auth.go"}
	got := fatesInput([]store.Run{{HeadSHA: sha('a'), Result: resultFor(t, sha('a'), s)}}, nil)

	want := map[string]bool{"db/query.go": true, "api/auth.go": true}
	if r := got.Reviews[0].Skills[0].Reviewed; !reflect.DeepEqual(r, want) {
		t.Errorf("Reviewed = %v; want %v", r, want)
	}
}

// ---------------------------------------------------------------------------
// The pure decisions: no database

func TestFateRetryDelay(t *testing.T) {
	for failed, want := range map[int]time.Duration{
		0: time.Minute, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 5: 16 * time.Minute,
	} {
		if got := fateRetryDelay(failed); got != want {
			t.Errorf("fateRetryDelay(%d) = %s; want %s", failed, got, want)
		}
	}
}

func TestFateStateEnded(t *testing.T) {
	for _, tc := range []struct {
		state  forge.ChangeState
		merged bool
		ended  bool
	}{
		{forge.StateMerged, true, true},
		{forge.StateClosed, false, true},
		{forge.StateOpen, false, false},
		{forge.StateLocked, false, false},
		{"", false, false},
	} {
		if merged, ended := fateStateEnded(tc.state); merged != tc.merged || ended != tc.ended {
			t.Errorf("fateStateEnded(%q) = %v, %v; want %v, %v", tc.state, merged, ended, tc.merged, tc.ended)
		}
	}
}

func TestFateEnd(t *testing.T) {
	boom := errors.New("boom")
	gone := fmt.Errorf("get: %w", forge.ErrNotFound)
	for _, tc := range []struct {
		name    string
		job     bool // the job's Merged
		change  forge.Change
		err     error
		merged  bool
		wantErr error // errors.Is; nil: none
		anyErr  bool  // any error
	}{
		{name: "merged on the platform, closed in the job", job: false, change: forge.Change{State: forge.StateMerged}, merged: true},
		{name: "closed on the platform, merged in the job", job: true, change: forge.Change{State: forge.StateClosed}, merged: false},
		{name: "gone takes the job", job: true, err: gone, merged: true},
		{name: "gone takes the job (closed)", job: false, err: gone, merged: false},
		{name: "open again", job: true, change: forge.Change{State: forge.StateOpen}, wantErr: errChangeOpen},
		{name: "a platform error", job: true, err: boom, wantErr: boom},
		{name: "locked is not over yet", job: true, change: forge.Change{State: forge.StateLocked}, anyErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := fateEnd(store.FateJob{Merged: tc.job}, tc.change, tc.err)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v; want %v", err, tc.wantErr)
				}
			case tc.anyErr:
				if err == nil || errors.Is(err, errChangeOpen) {
					t.Errorf("err = %v; want a failure to try again", err)
				}
			default:
				if err != nil || merged != tc.merged {
					t.Errorf("fateEnd = %v, %v; want %v, nil", merged, err, tc.merged)
				}
			}
		})
	}
}

// compareForge is a forge.Inline whose CompareDiff answers as set; embedding
// the interface makes it a forge.Forge too, without its other methods.
type compareForge struct {
	forge.Forge
	forge.Inline
	files []forge.FileDiff
	err   error
	calls int
}

func (c *compareForge) CompareDiff(_ context.Context, projectID int64, from, to string) ([]forge.FileDiff, error) {
	c.calls++
	return c.files, c.err
}

func TestFateCompareParsesFiles(t *testing.T) {
	cf := &compareForge{files: []forge.FileDiff{
		{Path: "db/query.go", Patch: "@@ -11,3 +11,2 @@\n a\n-x\n b"},
		{Path: "gone.go", Deleted: true},
		{Path: "new.go", OldPath: "old.go", Patch: "@@ -1 +1 @@\n-a\n+b"},
	}}
	got, err := fateCompare(context.Background(), cf, 7)(sha('a'), sha('b'))
	if err != nil {
		t.Fatal(err)
	}
	want := []diffmap.File{
		diffmap.Parse("db/query.go", "", cf.files[0].Patch, false),
		diffmap.Parse("gone.go", "", "", true),
		diffmap.Parse("new.go", "old.go", cf.files[2].Patch, false),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("files = %+v; want %+v", got, want)
	}
}

func TestFateCompareErrors(t *testing.T) {
	temporary := errors.New("503 from the platform")
	for _, tc := range []struct {
		name string
		err  error
		gone bool
	}{
		{"not found", fmt.Errorf("compare: %w", forge.ErrNotFound), true},
		{"not an ancestor on GitHub", fmt.Errorf("compare: %w", github.ErrNotAncestor), true},
		{"a partial diff on GitHub", fmt.Errorf("compare: %w", github.ErrPartialDiff), true},
		{"a partial diff on GitLab", fmt.Errorf("compare: %w", gitlab.ErrPartialDiff), true},
		{"a failure for now", temporary, false},
		{"a deadline", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fateCompare(context.Background(), &compareForge{err: tc.err}, 7)(sha('a'), sha('b'))
			if got := errors.Is(err, fates.ErrGone); got != tc.gone {
				t.Errorf("err = %v; ErrGone = %v, want %v", err, got, tc.gone)
			}
			if !tc.gone && !errors.Is(err, tc.err) {
				t.Errorf("err = %v; want the platform's error as it was", err)
			}
		})
	}
}

// A platform that can't compare never will.
func TestFateCompareNeedsInline(t *testing.T) {
	_, err := fateCompare(context.Background(), struct{ forge.Forge }{}, 7)(sha('a'), sha('b'))
	if !errors.Is(err, fates.ErrGone) {
		t.Errorf("err = %v; want ErrGone", err)
	}
}

func TestFateRows(t *testing.T) {
	got := fateRows([]fates.Fate{
		{Fingerprint: "a", Skill: "security", SkillHash: "h", Severity: "blocking", Fate: fates.Fixed},
		{Fingerprint: "b", Skill: "style", Severity: "suggestion", Fate: fates.WentAway},
	})
	want := []store.FindingFate{
		{Fingerprint: "a", Skill: "security", SkillHash: "h", Severity: "blocking", Fate: "fixed"},
		{Fingerprint: "b", Skill: "style", Severity: "suggestion", Fate: "went_away"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fateRows = %+v; want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// The loop and the sweep: these need the test database

// decide moves a run of head on merge request iid through dispatch and
// publication, with an outcome, so it is a decided review. The merge request
// is in the repository path.
func decide(t *testing.T, env *testEnv, mr store.MRKey, path, head string, res []byte) string {
	t.Helper()
	id, _, err := env.Store.EnqueueRun(env.ctx, mr, path, head, "main")
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	if _, err := env.Store.ClaimQueued(env.ctx, 100, env.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.MarkDispatched(env.ctx, id, "job-"+id, sha('b'), sha('5'), hashToken("seed-token"), env.Now().Add(time.Hour), env.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := env.Store.StoreResult(env.ctx, id, "seed-token", res, env.Now()); err != nil || out != store.ResultStored {
		t.Fatalf("StoreResult = %v, %v", out, err)
	}
	if err := env.Store.MarkPublished(env.ctx, id, store.RunDecision{Outcome: store.OutcomeBlocking, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	return id
}

// reviewOf decides a review of iid at head, with the security skill's findings.
func (e *testEnv) reviewOf(t *testing.T, iid int64, head string, findings ...result.Finding) {
	t.Helper()
	verdict := "no_blocking_issues"
	if len(findings) > 0 {
		verdict = "has_blocking_issues"
	}
	decide(t, e, e.MRKey(iid), testProjectPath, head, resultFor(t, head, fateSkill(t, verdict, findings...)))
}

// serveChange serves merge request iid in state ("opened", "merged" or
// "closed"), opened by author.
func serveChange(env *testEnv, iid int64, head, state string, author int64) {
	mr := openMR(iid, head)
	mr.State = state
	mr.Author = gitlab.User{ID: author, Username: "maya"}
	env.GitLab.SetMR(mr)
}

// removesLine12 is a compare in which line 12 of db/query.go was removed.
var removesLine12 = fileDiff{OldPath: "db/query.go", NewPath: "db/query.go", Diff: "@@ -11,3 +11,2 @@\n a\n-x\n b"}

// requestFates records that merge request iid ended, as a webhook does.
func requestFates(t *testing.T, env *testEnv, iid int64, merged bool) {
	t.Helper()
	if err := env.Store.RequestFates(env.ctx, env.MRKey(iid), testProjectPath, merged, env.Now()); err != nil {
		t.Fatal(err)
	}
}

type storedFate struct {
	Fingerprint, Skill, Severity, Fate string
	Merged, ByBot                      bool
}

// fateDB runs fn on a connection to the test schema.
func fateDB(t *testing.T, env *testEnv, fn func(conn *pgx.Conn)) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv(testDBEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.schema
	conn, err := pgx.ConnectConfig(env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(env.ctx)
	fn(conn)
}

// storedFates returns the fates stored for merge request iid, by fingerprint.
func storedFates(t *testing.T, env *testEnv, iid int64) []storedFate {
	t.Helper()
	var out []storedFate
	fateDB(t, env, func(conn *pgx.Conn) {
		rows, err := conn.Query(env.ctx, `SELECT fingerprint, skill, severity, fate, merged, by_bot FROM finding_fates
			WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 ORDER BY fingerprint`, testInstance, testProjectID, iid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var f storedFate
			if err := rows.Scan(&f.Fingerprint, &f.Skill, &f.Severity, &f.Fate, &f.Merged, &f.ByBot); err != nil {
				t.Fatal(err)
			}
			out = append(out, f)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	return out
}

type fateJobRow struct {
	Found, Finished bool
	Merged          bool
	Attempts        int
	NotBefore       *time.Time
}

// fateJobOf reads the fate job of merge request iid.
func fateJobOf(t *testing.T, env *testEnv, iid int64) fateJobRow {
	t.Helper()
	var j fateJobRow
	fateDB(t, env, func(conn *pgx.Conn) {
		var finished *time.Time
		err := conn.QueryRow(env.ctx, `SELECT merged, attempts, not_before, finished_at FROM fate_jobs
			WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, testInstance, testProjectID, iid).
			Scan(&j.Merged, &j.Attempts, &j.NotBefore, &finished)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		j.Found, j.Finished = true, finished != nil
	})
	return j
}

func workFates(t *testing.T, env *testEnv) {
	t.Helper()
	env.Engine.workFates(env.ctx)
}

const fpFirst = "aaaaaaaaaaaa"

func TestWorkFatesStoresFates(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12), fateFinding("bbbbbbbbbbbb", "suggestion", 40))
	env.reviewOf(t, 1, sha('c'))
	if _, err := env.Store.Dismiss(env.ctx, store.Dismissal{MR: env.MRKey(1), Fingerprint: "bbbbbbbbbbbb", Severity: "suggestion", Actor: "maya"}); err != nil {
		t.Fatal(err)
	}
	serveChange(env, 1, sha('c'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
	env.GitLab.ServeCompare(removesLine12)
	requestFates(t, env, 1, true)

	workFates(t, env)

	want := []storedFate{
		{Fingerprint: fpFirst, Skill: "security", Severity: "blocking", Fate: "fixed", Merged: true},
		{Fingerprint: "bbbbbbbbbbbb", Skill: "security", Severity: "suggestion", Fate: "dismissed", Merged: true},
	}
	if got := storedFates(t, env, 1); !reflect.DeepEqual(got, want) {
		t.Errorf("fates = %+v; want %+v\n%s", got, want, env.Logs())
	}
	if j := fateJobOf(t, env, 1); !j.Finished {
		t.Errorf("job = %+v; want it finished", j)
	}
	// Worked out once: another pass finds nothing to do.
	env.GitLab.Reset()
	workFates(t, env)
	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("a pass with nothing to do made %d platform calls", len(reqs))
	}
}

// The platform, not the webhook, says how the change ended.
func TestWorkFatesTakesTheEndFromThePlatform(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "closed", 55)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
	requestFates(t, env, 1, true) // a late webhook said merged

	workFates(t, env)

	got := storedFates(t, env, 1)
	if len(got) != 1 || got[0].Merged || got[0].Fate != "open" {
		t.Errorf("fates = %+v; want one open finding of a closed change", got)
	}
}

// A change nobody reviewed has no findings: its job is finished with no
// platform call.
func TestWorkFatesNeverReviewedCostsNothing(t *testing.T) {
	env := newTestEnv(t)
	requestFates(t, env, 1, true)

	workFates(t, env)

	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("%d platform calls for a change never reviewed", len(reqs))
	}
	if j := fateJobOf(t, env, 1); !j.Found || !j.Finished {
		t.Errorf("job = %+v; want it finished", j)
	}
}

func TestWorkFatesReopenedDropsTheJob(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "opened", 55)
	requestFates(t, env, 1, false)

	workFates(t, env)

	if j := fateJobOf(t, env, 1); j.Found {
		t.Errorf("job = %+v; want it dropped", j)
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none", got)
	}
}

func TestWorkFatesTransientErrorRetriesAndStoresNothing(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.reviewOf(t, 1, sha('c'))
	serveChange(env, 1, sha('c'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, comparePath(), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	requestFates(t, env, 1, true)
	start := env.Now()

	workFates(t, env)

	j := fateJobOf(t, env, 1)
	if !j.Found || j.Finished || j.Attempts != 1 || j.NotBefore == nil || !j.NotBefore.Equal(start.Add(time.Minute)) {
		t.Errorf("job = %+v; want unfinished, 1 attempt, held until %s", j, start.Add(time.Minute))
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none", got)
	}

	// Held back for a minute.
	env.GitLab.Reset()
	workFates(t, env)
	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("the job was tried again at once: %d calls", len(reqs))
	}

	// Then it works, and the platform recovers.
	env.SetNow(start.Add(time.Minute))
	env.GitLab.ServeCompare(removesLine12)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
	workFates(t, env)
	got := storedFates(t, env, 1)
	if len(got) != 1 || got[0].Fate != "fixed" {
		t.Errorf("fates after the retry = %+v; want the finding fixed\n%s", got, env.Logs())
	}
}

func TestWorkFatesStopsAfterFiveAttempts(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.reviewOf(t, 1, sha('c'))
	serveChange(env, 1, sha('c'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, comparePath(), http.StatusForbidden, `{"message":"403 Forbidden"}`)
	requestFates(t, env, 1, true)

	wantWait := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for attempt := 1; attempt <= 5; attempt++ {
		workFates(t, env)
		j := fateJobOf(t, env, 1)
		if j.Attempts != attempt || j.Finished {
			t.Fatalf("after attempt %d: job = %+v", attempt, j)
		}
		if attempt <= len(wantWait) {
			if want := env.Now().Add(wantWait[attempt-1]); j.NotBefore == nil || !j.NotBefore.Equal(want) {
				t.Errorf("after attempt %d: held until %v; want %s", attempt, j.NotBefore, want)
			}
		}
		env.SetNow(env.Now().Add(time.Hour))
	}

	env.GitLab.Reset()
	workFates(t, env)
	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("a stopped job was tried a sixth time: %d calls", len(reqs))
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none: no guessing", got)
	}
	if !strings.Contains(env.Logs(), "level=WARN") || !strings.Contains(env.Logs(), "giving up") {
		t.Errorf("no warning that the change is given up on\n%s", env.Logs())
	}
}

func TestWorkFatesForcePushIsWentAway(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.reviewOf(t, 1, sha('c'))
	serveChange(env, 1, sha('c'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
	// The first head is gone after a force-push.
	env.GitLab.Reply(http.MethodGet, comparePath(), http.StatusNotFound, `{"message":"404 Commit Not Found"}`)
	requestFates(t, env, 1, true)

	workFates(t, env)

	got := storedFates(t, env, 1)
	if len(got) != 1 || got[0].Fate != "went_away" {
		t.Errorf("fates = %+v; want one went_away\n%s", got, env.Logs())
	}
}

func TestWorkFatesBotAuthor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(env *testEnv)
		byBot bool
	}{
		{"a bot", func(env *testEnv) {
			env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": true})
		}, true},
		{"a person", func(env *testEnv) {
			env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
		}, false},
		// When it can't be told, the change counts as a person's.
		{"unknown", func(env *testEnv) {
			env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusForbidden, `{"message":"403 Forbidden"}`)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
			serveChange(env, 1, sha('a'), "merged", 55)
			tc.serve(env)
			requestFates(t, env, 1, true)

			workFates(t, env)

			got := storedFates(t, env, 1)
			if len(got) != 1 || got[0].ByBot != tc.byBot {
				t.Errorf("fates = %+v; want by_bot %v\n%s", got, tc.byBot, env.Logs())
			}
		})
	}
}

// A pass stops claiming work once its budget is spent, and a hung platform
// holds only the job it hangs on.
func TestWorkFatesBudget(t *testing.T) {
	defer func(b time.Duration) { fateBudget = b }(fateBudget)
	fateBudget = 300 * time.Millisecond

	env := newTestEnv(t)
	for iid := int64(1); iid <= 2; iid++ {
		env.reviewOf(t, iid, sha('a'), fateFinding(fpFirst, "blocking", 12))
		requestFates(t, env, iid, true)
	}
	env.GitLab.Handle(http.MethodGet, mrPath(1), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	serveChange(env, 2, sha('a'), "merged", 55)

	start := time.Now()
	workFates(t, env)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a pass took %s with a hung platform, want about the %s budget", took, fateBudget)
	}

	// The hung one is a temporary failure; the other was never claimed.
	if j := fateJobOf(t, env, 1); j.Attempts != 1 || j.Finished {
		t.Errorf("hung job = %+v; want 1 attempt, unfinished", j)
	}
	if j := fateJobOf(t, env, 2); j.Attempts != 0 || j.Finished {
		t.Errorf("second job = %+v; want it left for the next pass", j)
	}
}

func TestFatesSweepFindsAMissedMerge(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})

	env.Engine.sweepFates(env.ctx)

	// The sweep only asks for the work.
	if j := fateJobOf(t, env, 1); !j.Found || j.Finished || !j.Merged {
		t.Fatalf("job after the sweep = %+v; want a pending merge\n%s", j, env.Logs())
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Fatalf("the sweep stored fates itself: %+v", got)
	}

	workFates(t, env)
	got := storedFates(t, env, 1)
	if len(got) != 1 || got[0].Fate != "open" || !got[0].Merged {
		t.Errorf("fates = %+v; want one open finding of a merged change", got)
	}
}

// A closed change is requested too; an open one is only marked as looked at.
func TestFatesSweepOpenChangeIsLeftAlone(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "opened", 55)

	env.Engine.sweepFates(env.ctx)

	if j := fateJobOf(t, env, 1); j.Found {
		t.Errorf("job = %+v; want none for an open change", j)
	}
	if reqs := env.GitLab.Requests(http.MethodGet, mrPath(1)); len(reqs) != 1 {
		t.Errorf("%d reads of the change; want 1", len(reqs))
	}
	// Looked at: it goes behind the others, not forgotten.
	changes, err := env.Store.ChangesWithoutFates(env.ctx, env.Now().Add(-time.Hour), 50)
	if err != nil || len(changes) != 1 {
		t.Fatalf("ChangesWithoutFates = %+v, %v; want the change still offered", changes, err)
	}
}

// A repository not switched on is marked as looked at, with no platform call.
func TestFatesSweepRepositoryNotSwitchedOn(t *testing.T) {
	env := newTestEnv(t)
	other := store.MRKey{Instance: testInstance, ProjectID: 9, IID: 1}
	decide(t, env, other, "acme/other", sha('a'), resultFor(t, sha('a'), fateSkill(t, "no_blocking_issues")))

	env.Engine.sweepFates(env.ctx)

	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("%d platform calls for a repository that isn't switched on", len(reqs))
	}
	fateDB(t, env, func(conn *pgx.Conn) {
		var n int
		if err := conn.QueryRow(env.ctx, `SELECT count(*) FROM fate_checks WHERE project_id = 9`).Scan(&n); err != nil || n != 1 {
			t.Errorf("fate_checks rows = %d, %v; want the change marked as looked at", n, err)
		}
		if err := conn.QueryRow(env.ctx, `SELECT count(*) FROM fate_jobs`).Scan(&n); err != nil || n != 0 {
			t.Errorf("fate_jobs rows = %d, %v; want none", n, err)
		}
	})
}

// One platform call per repository per sweep, however many of its changes
// have a review and no fates; the next sweep looks at another.
func TestFatesSweepOneCallPerRepository(t *testing.T) {
	env := newTestEnv(t)
	for iid := int64(1); iid <= 3; iid++ {
		env.reviewOf(t, iid, sha('a'), fateFinding(fpFirst, "blocking", 12))
		serveChange(env, iid, sha('a'), "opened", 55)
	}

	read := map[int64]bool{}
	for sweep := 1; sweep <= 3; sweep++ {
		env.GitLab.Reset()
		env.Engine.sweepFates(env.ctx)
		var calls int
		for iid := int64(1); iid <= 3; iid++ {
			n := len(env.GitLab.Requests(http.MethodGet, mrPath(iid)))
			calls += n
			if n > 0 {
				if read[iid] {
					t.Errorf("sweep %d read change %d again before the others", sweep, iid)
				}
				read[iid] = true
			}
		}
		if calls != 1 {
			t.Errorf("sweep %d made %d reads of changes; want 1 for the one repository", sweep, calls)
		}
	}
	if len(read) != 3 {
		t.Errorf("three sweeps read %d different changes; want all 3", len(read))
	}
}

func TestPruneFatesDeletesAYearOld(t *testing.T) {
	env := newTestEnv(t)
	old := env.Now().Add(-fateRetention - time.Hour)
	if err := env.Store.RequestFates(env.ctx, env.MRKey(1), testProjectPath, true, old); err != nil {
		t.Fatal(err)
	}
	requestFates(t, env, 2, true)

	env.Engine.pruneFates(env.ctx)

	if j := fateJobOf(t, env, 1); j.Found {
		t.Errorf("an old job was kept: %+v", j)
	}
	if j := fateJobOf(t, env, 2); !j.Found {
		t.Error("a recent job was pruned")
	}
}
