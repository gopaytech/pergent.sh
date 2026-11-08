package callback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	runID = "11111111-1111-1111-1111-111111111111"
	base  = "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
	head  = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func resultJSON(b, h string) string {
	return `{"schema_version":1,"status":"error","base":"` + b + `","head":"` + h + `",` +
		`"error":{"code":"internal","message":"x"},"comment":{"markdown":"hi"}}`
}

// fake is a Results with one run, one good token and a scripted outcome.
type fake struct {
	outcome  store.ResultOutcome
	storeErr error

	token    string // the token that StoreResult accepts
	known    bool
	run      store.Run
	runErr   error
	replaced []string
	reported []string
	stored   int
	replErr  error

	recordErr error
	recorded  []recordedUsage
	onRecord  func() // called as RecordUsage starts
	recordCtx error  // the context's error once onRecord has returned
}

// recordedUsage is one RecordUsage call.
type recordedUsage struct {
	id     string
	skills []store.SkillUsage
}

func (f *fake) Run(_ context.Context, id string) (store.Run, bool, error) {
	if f.runErr != nil {
		return store.Run{}, false, f.runErr
	}
	return f.run, f.known, nil
}

func (f *fake) StoreResult(_ context.Context, id, token string, _ []byte, _ time.Time) (store.ResultOutcome, error) {
	if f.storeErr != nil {
		return 0, f.storeErr
	}
	if !f.known || token != f.token {
		return store.ResultUnauthorized, nil
	}
	if f.outcome == store.ResultStored {
		f.stored++
	}
	return f.outcome, nil
}

func (f *fake) ReportFailure(_ context.Context, id, reason string, _ time.Time) (bool, error) {
	f.reported = append(f.reported, reason)
	return true, nil
}

func (f *fake) ReplaceWithFailure(_ context.Context, id, reason string) error {
	f.replaced = append(f.replaced, reason)
	return f.replErr
}

func (f *fake) RecordUsage(ctx context.Context, id string, skills []store.SkillUsage) error {
	if f.onRecord != nil {
		f.onRecord()
	}
	f.recordCtx = ctx.Err()
	f.recorded = append(f.recorded, recordedUsage{id, skills})
	return f.recordErr
}

func newFake() *fake {
	return &fake{known: true, token: "good", run: store.Run{ID: runID, BaseSHA: base, HeadSHA: head}}
}

type env struct {
	f       *fake
	woken   int
	logs    bytes.Buffer
	h       http.Handler
	handler *Handler
}

func newEnv(f *fake) *env {
	e := &env{f: f}
	h := &Handler{Results: f, Now: func() time.Time { return now }, Wake: func() { e.woken++ }, Log: slog.New(slog.NewTextHandler(&e.logs, nil))}
	mux := http.NewServeMux()
	mux.Handle("POST /runs/{id}/result", h)
	mux.Handle("/runs/{id}/result", h)
	e.h, e.handler = mux, h
	return e
}

func (e *env) post(id, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/result", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	e.handler.usageWork.Wait()
	return rec
}

func TestValidResultStored(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", resultJSON(base, head))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if e.woken != 1 || e.f.stored != 1 || len(e.f.replaced) != 0 {
		t.Errorf("woken=%d stored=%d replaced=%v", e.woken, e.f.stored, e.f.replaced)
	}
}

func TestMissingBearer401(t *testing.T) {
	for _, auth := range []string{"", "Basic good", "good", "Bearer "} {
		e := newEnv(newFake())
		if rec := e.post(runID, auth, resultJSON(base, head)); rec.Code != 401 {
			t.Errorf("auth %q: status = %d", auth, rec.Code)
		}
		if e.f.stored != 0 || e.woken != 0 {
			t.Errorf("auth %q: stored=%d woken=%d", auth, e.f.stored, e.woken)
		}
	}
}

func TestWrongToken401(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer bad", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.reported)+len(e.f.replaced) != 0 || e.woken != 0 {
		t.Errorf("reported=%v replaced=%v woken=%d", e.f.reported, e.f.replaced, e.woken)
	}
}

func TestDuplicate409(t *testing.T) {
	f := newFake()
	f.outcome = store.ResultConflict
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 409 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 || len(f.replaced) != 0 {
		t.Errorf("woken=%d replaced=%v", e.woken, f.replaced)
	}
}

func TestSuperseded200(t *testing.T) {
	f := newFake()
	f.outcome = store.ResultDiscarded
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 || len(f.replaced) != 0 {
		t.Errorf("woken=%d replaced=%v", e.woken, f.replaced)
	}
}

func TestTooLarge413(t *testing.T) {
	e := newEnv(newFake())
	big := strings.Repeat("a", 11<<20+1)
	if rec := e.post(runID, "Bearer good", big); rec.Code != 413 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.f.stored != 0 {
		t.Errorf("stored a too-large body")
	}
}

func TestHeadMismatchFailsRun(t *testing.T) {
	e := newEnv(newFake())
	other := strings.Repeat("a", 40)
	rec := e.post(runID, "Bearer good", resultJSON(base, other))
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "different commit") ||
		!strings.HasPrefix(e.f.replaced[0], "pergent sent a result pergent.sh couldn't use: ") {
		t.Errorf("replaced = %v", e.f.replaced)
	}
	if e.woken != 1 {
		t.Errorf("woken = %d", e.woken)
	}
}

func TestBaseMismatchFailsRun(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", resultJSON(strings.Repeat("b", 40), head))
	if rec.Code != 400 || len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "different commit") {
		t.Errorf("status = %d replaced = %v", rec.Code, e.f.replaced)
	}
}

func TestUnparseableResultFailsRun(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", `{"schema_version":99}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "schema_version") || e.woken != 1 {
		t.Errorf("replaced = %v woken = %d", e.f.replaced, e.woken)
	}
}

func TestInvalidJSONWithGoodTokenFailsRun(t *testing.T) {
	f := newFake()
	f.storeErr = store.ErrInvalidResult
	e := newEnv(f)
	rec := e.post(runID, "Bearer good", `not json`)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(f.reported) != 1 || !strings.HasPrefix(f.reported[0], "pergent sent a result pergent.sh couldn't use: ") || e.woken != 1 {
		t.Errorf("reported = %v woken = %d", f.reported, e.woken)
	}
}

func TestGarbageWithBadTokenIs401(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer bad", `not json at all`); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(e.f.reported)+len(e.f.replaced) != 0 || e.woken != 0 {
		t.Errorf("reported=%v replaced=%v woken=%d", e.f.reported, e.f.replaced, e.woken)
	}
}

func TestUnknownRun401(t *testing.T) {
	f := newFake()
	f.known = false
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec := e.post("not-a-uuid", "Bearer good", resultJSON(base, head)); rec.Code != 401 {
		t.Fatalf("malformed id: status = %d", rec.Code)
	}
}

func TestNonPost405(t *testing.T) {
	e := newEnv(newFake())
	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/result", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestStorageErrorIs500AndLeavesRetry(t *testing.T) {
	f := newFake()
	f.storeErr = errors.New("db down")
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if e.woken != 0 {
		t.Errorf("woken on error")
	}
}

func TestReplaceErrorIs500(t *testing.T) {
	f := newFake()
	f.replErr = errors.New("db down")
	e := newEnv(f)
	if rec := e.post(runID, "Bearer good", resultJSON(base, strings.Repeat("a", 40))); rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTokenNeverInResponse(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer secret-token-value", resultJSON(base, head))
	b, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(b), "secret-token-value") {
		t.Errorf("token echoed: %s", b)
	}
}

// The merge markers are pergent.sh's own: a result a Job delivers with one is
// unusable, so a Job can't pass a partial result off as merged.
func TestMergeMarkersInDeliveredResultFailRun(t *testing.T) {
	for name, extra := range map[string]string{
		"merged":     `"merged":true,`,
		"merge note": `"merge_note":"x",`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(newFake())
			body := strings.Replace(resultJSON(base, head), `{"schema_version":1,`, `{"schema_version":1,`+extra, 1)
			rec := e.post(runID, "Bearer good", body)
			if rec.Code != 400 || len(e.f.replaced) != 1 || !strings.Contains(e.f.replaced[0], "merge") {
				t.Errorf("status = %d replaced = %v", rec.Code, e.f.replaced)
			}
		})
	}
}

// completedJSON is a completed result of the named skills (a name may repeat).
func completedJSON(names ...string) string {
	var skills []string
	for _, n := range names {
		skills = append(skills, `{"name":"`+n+`","verdict":"no_blocking_issues","findings":[]}`)
	}
	return `{"schema_version":1,"status":"completed","base":"` + base + `","head":"` + head + `",` +
		`"skills":[` + strings.Join(skills, ",") + `],"comment":{"markdown":"hi"}}`
}

// A completed result has to cover the skills the Job was started with, no
// more, no fewer, each once: a repeated name can't stand in for a missing one.
func TestResultOfOtherSkillsThanDispatchedFailsRun(t *testing.T) {
	sources := map[string]string{"security": "library", "style": "repo"}
	for name, tc := range map[string]struct {
		run    store.Run
		body   string
		wantOK bool
	}{
		"all skills":         {store.Run{SkillSources: sources}, completedJSON("style", "security"), true},
		"a skill missing":    {store.Run{SkillSources: sources}, completedJSON("security"), false},
		"a duplicate":        {store.Run{SkillSources: sources}, completedJSON("security", "security"), false},
		"a duplicate hiding": {store.Run{SkillSources: sources}, completedJSON("security", "security", "style"), false},
		"an extra skill":     {store.Run{SkillSources: sources}, completedJSON("security", "style", "other"), false},
		"the subset":         {store.Run{SkillSources: sources, SkillSubset: []string{"style"}}, completedJSON("style"), true},
		"more than subset":   {store.Run{SkillSources: sources, SkillSubset: []string{"style"}}, completedJSON("style", "security"), false},
		"subset duplicated":  {store.Run{SkillSources: sources, SkillSubset: []string{"style", "security"}}, completedJSON("style", "style"), false},
		// Nothing recorded (an older run, or recording failed): not checked.
		"nothing recorded": {store.Run{}, completedJSON("anything"), true},
		// An error result has no skills to check.
		"error result": {store.Run{SkillSources: sources}, resultJSON(base, head), true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			tc.run.ID, tc.run.BaseSHA, tc.run.HeadSHA = runID, base, head
			f.run = tc.run
			e := newEnv(f)
			rec := e.post(runID, "Bearer good", tc.body)
			if tc.wantOK {
				if rec.Code != 200 || len(f.replaced) != 0 {
					t.Errorf("status = %d replaced = %v; want it accepted", rec.Code, f.replaced)
				}
				return
			}
			if rec.Code != 400 || len(f.replaced) != 1 || !strings.Contains(f.replaced[0], "skill") {
				t.Errorf("status = %d replaced = %v; want the run failed", rec.Code, f.replaced)
			}
		})
	}
}

// The impact check is not one of the dispatched skills: it is expected when the
// run recorded linked repositories, and refused when it did not, so a result
// can't gain a skill that nothing started. A result that lacks it is not
// refused: the publisher writes a failed one (an image that doesn't know the
// feature ignores the links).
func TestImpactSkillAgainstTheRecordedLinks(t *testing.T) {
	sources := map[string]string{"security": "library"}
	own := map[string]string{"security": "library", "impact": "repo"}
	links := []store.Link{{Target: "acme/api"}}
	for name, tc := range map[string]struct {
		run    store.Run
		body   string
		wantOK bool
	}{
		"links and impact":          {store.Run{SkillSources: sources, LinkedRepos: links}, completedJSON("security", "impact"), true},
		"links and no impact":       {store.Run{SkillSources: sources, LinkedRepos: links}, completedJSON("security"), true},
		"no links and impact":       {store.Run{SkillSources: sources}, completedJSON("security", "impact"), false},
		"no links and no impact":    {store.Run{SkillSources: sources}, completedJSON("security"), true},
		"impact twice":              {store.Run{SkillSources: sources, LinkedRepos: links}, completedJSON("security", "impact", "impact"), false},
		"nothing recorded, impact":  {store.Run{}, completedJSON("security", "impact"), false},
		"nothing recorded, links":   {store.Run{LinkedRepos: links}, completedJSON("anything", "impact"), true},
		"subset with impact":        {store.Run{SkillSources: sources, SkillSubset: []string{"security", "impact"}, LinkedRepos: links}, completedJSON("security", "impact"), true},
		"subset lacking impact":     {store.Run{SkillSources: sources, SkillSubset: []string{"security", "impact"}, LinkedRepos: links}, completedJSON("security"), true},
		"subset without links":      {store.Run{SkillSources: sources, SkillSubset: []string{"security"}}, completedJSON("security", "impact"), false},
		"impact and an unknown one": {store.Run{SkillSources: sources, LinkedRepos: links}, completedJSON("security", "impact", "other"), false},
		"links and a missing skill": {store.Run{SkillSources: sources, LinkedRepos: links}, completedJSON("impact"), false},
		// A repository's own skill named impact is an ordinary skill.
		"own impact, reported":          {store.Run{SkillSources: own}, completedJSON("security", "impact"), true},
		"own impact, missing":           {store.Run{SkillSources: own}, completedJSON("security"), false},
		"own impact, twice":             {store.Run{SkillSources: own}, completedJSON("security", "impact", "impact"), false},
		"own impact, subset":            {store.Run{SkillSources: own, SkillSubset: []string{"impact"}}, completedJSON("impact"), true},
		"own impact, subset, not asked": {store.Run{SkillSources: own, SkillSubset: []string{"security"}}, completedJSON("security", "impact"), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			tc.run.ID, tc.run.BaseSHA, tc.run.HeadSHA = runID, base, head
			f.run = tc.run
			rec := newEnv(f).post(runID, "Bearer good", tc.body)
			if tc.wantOK {
				if rec.Code != 200 || len(f.replaced) != 0 {
					t.Errorf("status = %d replaced = %v; want it accepted", rec.Code, f.replaced)
				}
				return
			}
			if rec.Code != 400 || len(f.replaced) != 1 || !strings.Contains(f.replaced[0], "skill") {
				t.Errorf("status = %d replaced = %v; want the run failed", rec.Code, f.replaced)
			}
		})
	}
}

// usageJSON is a completed result of two skills that carry their tokens.
func usageJSON() string {
	return `{"schema_version":1,"status":"completed","base":"` + base + `","head":"` + head + `",` +
		`"skills":[` +
		`{"name":"style","verdict":"no_blocking_issues","findings":[],"tokens":{"input":1200,"output":300}},` +
		`{"name":"security","verdict":"has_blocking_issues","findings":[{"severity":"blocking"}],"tokens":{"input":5000,"output":700}}` +
		`],"comment":{"markdown":"hi"}}`
}

func TestAcceptedResultRecordsItsUsage(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer good", usageJSON()); rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(e.f.recorded) != 1 || e.f.recorded[0].id != runID {
		t.Fatalf("recorded = %+v", e.f.recorded)
	}
	got := e.f.recorded[0].skills
	if len(got) != 2 || got[0].Skill != "style" || got[0].In != 1200 || got[1].Skill != "security" || got[1].In != 5000 {
		t.Errorf("skills = %+v", got)
	}
}

// Publishing is woken before the usage is written, so a slow database never
// holds it back.
func TestPublishingIsWokenBeforeUsageIsRecorded(t *testing.T) {
	f := newFake()
	e := newEnv(f)
	wokenAtRecord := -1
	f.onRecord = func() { wokenAtRecord = e.woken }
	if rec := e.post(runID, "Bearer good", usageJSON()); rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if wokenAtRecord != 1 {
		t.Errorf("woken = %d when usage was recorded; want 1, so before", wokenAtRecord)
	}
}

// The Job has its answer while the usage is still being written, so a slow
// database never holds it up. The write outlives the request: a Job that hangs
// up as soon as it has its answer doesn't lose the usage.
func TestAnswerDoesNotWaitForUsage(t *testing.T) {
	f := newFake()
	e := newEnv(f)
	release := make(chan struct{})
	f.onRecord = func() { <-release }

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	req := httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/result", strings.NewReader(usageJSON())).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		e.h.ServeHTTP(rec, req)
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the answer waited for the usage to be written")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}

	hangUp()
	close(release)
	e.handler.usageWork.Wait()
	if len(f.recorded) != 1 || f.recordCtx != nil {
		t.Errorf("recorded = %+v, context error = %v", f.recorded, f.recordCtx)
	}
}

// Recording runs outside the request now, where a panic would take the whole
// process with it.
func TestUsagePanicStaysInItsOwnWork(t *testing.T) {
	f := newFake()
	e := newEnv(f)
	f.onRecord = func() { panic("boom") }
	if rec := e.post(runID, "Bearer good", usageJSON()); rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if out := e.logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "usage") {
		t.Errorf("log = %q", out)
	}
}

// A result turned into a failure shows as a review with no result, so its
// tokens are unknown: nothing is recorded for it.
func TestUnusableResultRecordsNoUsage(t *testing.T) {
	e := newEnv(newFake())
	rec := e.post(runID, "Bearer good", strings.Replace(usageJSON(), head, strings.Repeat("a", 40), 1))
	if rec.Code != 400 || len(e.f.replaced) != 1 {
		t.Fatalf("status = %d replaced = %v", rec.Code, e.f.replaced)
	}
	if len(e.f.recorded) != 0 {
		t.Errorf("recorded = %+v", e.f.recorded)
	}
}

func TestRecordUsageFailureNeverChangesTheAnswer(t *testing.T) {
	f := newFake()
	f.recordErr = errors.New("database is down")
	e := newEnv(f)
	rec := e.post(runID, "Bearer good", usageJSON())
	if rec.Code != 200 || e.woken != 1 || len(f.replaced) != 0 {
		t.Errorf("status = %d woken = %d replaced = %v", rec.Code, e.woken, f.replaced)
	}
	if out := e.logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "usage") {
		t.Errorf("log = %q", out)
	}
}

// A body usage can't read still marks the run, so it isn't read again later.
func TestUnreadableUsageStillMarksTheRun(t *testing.T) {
	e := newEnv(newFake())
	if rec := e.post(runID, "Bearer good", resultJSON(base, head)); rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(e.f.recorded) != 1 || e.f.recorded[0].skills != nil {
		t.Errorf("recorded = %+v", e.f.recorded)
	}
	if !strings.Contains(e.logs.String(), "level=WARN") {
		t.Errorf("log = %q", e.logs.String())
	}
}
