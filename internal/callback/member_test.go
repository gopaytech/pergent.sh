package callback

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	memberID = "22222222-2222-2222-2222-222222222222"
	otherRun = "33333333-3333-3333-3333-333333333333"
)

// fakeMembers is a MemberResults with one review (run runID) of one member,
// one good token and a scripted outcome.
type fakeMembers struct {
	outcome  store.ResultOutcome
	storeErr error
	token    string
	members  map[string][]store.PanelMember // by the run of the path
	run      store.Run
	waits    bool

	stored   []string // the member of each stored result
	replaced []store.Failure
	reported []store.Failure
	replErr  error
}

func (f *fakeMembers) PanelRun(_ context.Context, root string) (store.Run, bool, error) {
	if root != f.run.ID {
		return store.Run{}, false, nil
	}
	return f.run, f.waits, nil
}

func (f *fakeMembers) StoreMemberResult(_ context.Context, runID, id, token string, body []byte, _ time.Time) (store.ResultOutcome, error) {
	if f.storeErr != nil {
		return 0, f.storeErr
	}
	if !f.memberOf(runID, id) || token != f.token {
		return store.ResultUnauthorized, nil
	}
	if f.outcome == store.ResultStored {
		f.stored = append(f.stored, id)
	}
	return f.outcome, nil
}

func (f *fakeMembers) ReportMemberFailure(_ context.Context, id string, fail store.Failure, _ time.Time) (bool, error) {
	f.reported = append(f.reported, fail)
	return true, nil
}

func (f *fakeMembers) ReplaceMemberWithFailure(_ context.Context, id string, fail store.Failure) error {
	f.replaced = append(f.replaced, fail)
	return f.replErr
}

// memberOf reports whether member id is one of run runID's.
func (f *fakeMembers) memberOf(runID, id string) bool {
	for _, m := range f.members[runID] {
		if m.ID == id {
			return true
		}
	}
	return false
}

func newFakeMembers() *fakeMembers {
	return &fakeMembers{
		outcome: store.ResultStored, token: "good", waits: true,
		run: store.Run{ID: runID, BaseSHA: base, HeadSHA: head},
		members: map[string][]store.PanelMember{
			runID: {{ID: memberID, RunID: runID, Role: "a", State: store.MemberRunning}},
			// Another review with a member of its own.
			otherRun: {{ID: "44444444-4444-4444-4444-444444444444", RunID: otherRun, Role: "a"}},
		},
	}
}

type memberEnv struct {
	f     *fakeMembers
	woken int
	logs  bytes.Buffer
	mux   *http.ServeMux
}

func newMemberEnv(f *fakeMembers) *memberEnv {
	e := &memberEnv{f: f}
	h := &MemberHandler{Members: f, Now: func() time.Time { return now }, Wake: func() { e.woken++ },
		Log: slog.New(slog.NewTextHandler(&e.logs, nil))}
	e.mux = http.NewServeMux()
	e.mux.Handle("POST /runs/{id}/members/{member}/result", h)
	e.mux.Handle("/runs/{id}/members/{member}/result", h)
	return e
}

func (e *memberEnv) post(run, member, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/runs/"+run+"/members/"+member+"/result", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// A good token and a usable result: the member's result is stored, the
// engine is woken, and the run is not touched.
func TestMemberHandlerStoresResult(t *testing.T) {
	e := newMemberEnv(newFakeMembers())
	rec := e.post(runID, memberID, "Bearer good", completedJSON("security"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(e.f.stored) != 1 || len(e.f.replaced) != 0 || len(e.f.reported) != 0 || e.woken != 1 {
		t.Errorf("stored=%v replaced=%v reported=%v woken=%d", e.f.stored, e.f.replaced, e.f.reported, e.woken)
	}
	if strings.Contains(e.logs.String(), base) || strings.Contains(e.logs.String(), "good") {
		t.Errorf("the body or the token reached the log:\n%s", e.logs.String())
	}
}

// A missing or wrong token is 401, and nothing is stored or failed.
func TestMemberHandlerBadToken(t *testing.T) {
	for _, auth := range []string{"", "Bearer bad", "Basic good"} {
		e := newMemberEnv(newFakeMembers())
		rec := e.post(runID, memberID, auth, completedJSON("security"))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", auth, rec.Code)
		}
		if len(e.f.stored)+len(e.f.replaced)+len(e.f.reported) != 0 || e.woken != 0 {
			t.Errorf("%q: stored=%v replaced=%v reported=%v woken=%d", auth, e.f.stored, e.f.replaced, e.f.reported, e.woken)
		}
		if strings.Contains(rec.Body.String(), "good") {
			t.Errorf("%q: the token is in the answer", auth)
		}
	}
}

// A result for another commit fails the member, never the run.
func TestMemberHandlerWrongCommit(t *testing.T) {
	e := newMemberEnv(newFakeMembers())
	rec := e.post(runID, memberID, "Bearer good", resultJSON(base, "deadbeef"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	want := unusable + "the result is for a different commit than the one this run reviews"
	if len(e.f.replaced) != 1 || e.f.replaced[0].Reason != want || e.f.replaced[0].Step != "delivery" {
		t.Errorf("replaced = %+v; want %q at delivery", e.f.replaced, want)
	}
	if e.woken != 1 {
		t.Errorf("woken = %d, want 1", e.woken)
	}
}

// The engine's settle pass ran the same check first and failed the member
// (ErrNotReported): the answer is still the check's 400, not an error.
func TestMemberHandlerSettledFirst(t *testing.T) {
	f := newFakeMembers()
	f.replErr = store.ErrNotReported
	e := newMemberEnv(f)
	rec := e.post(runID, memberID, "Bearer good", resultJSON(base, "deadbeef"))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a different commit") {
		t.Fatalf("status = %d %q, want the check's 400", rec.Code, rec.Body.String())
	}
}

// CheckStored is the handlers' check of a stored body: it parses and runs
// checkResult, and gives the failure to record in the result's place.
func TestCheckStored(t *testing.T) {
	run := store.Run{BaseSHA: base, HeadSHA: head, SkillSubset: []string{"security"}}
	if res, f, ok := CheckStored([]byte(completedJSON("security")), run); !ok || res == nil || res.Status != "completed" || f != (store.Failure{}) {
		t.Errorf("good result = %v, %+v, %v; want ok", res, f, ok)
	}
	for body, want := range map[string]string{
		resultJSON(base, "x"): "the result is for a different commit than the one this run reviews",
		`{}`:                  "unsupported result schema_version 0 (want 1)",
	} {
		res, f, ok := CheckStored([]byte(body), run)
		if ok || res != nil || f.Reason != unusable+want || f.Step != "delivery" {
			t.Errorf("CheckStored(%s) = %v, %+v, %v; want %q at delivery", body, res, f, ok, want)
		}
	}
}

// A completed result with a skill the member's Job was not started with, or
// without one it was, fails the member, with the run's own check against
// the skills of a whole review (recorded at the split) or of a re-run.
func TestMemberHandlerUnknownSkill(t *testing.T) {
	whole := func(r *store.Run) { r.SkillSources = map[string]string{"security": "library"} }
	subset := func(r *store.Run) { r.SkillSubset = []string{"security"} }
	for name, set := range map[string]func(*store.Run){"whole review": whole, "re-run": subset} {
		for body, want := range map[string]string{
			completedJSON("security", "style"): "the result has skills the Job was not started with",
			completedJSON("style"):             `the result lacks the skills the Job was started with ("security")`,
		} {
			f := newFakeMembers()
			set(&f.run)
			e := newMemberEnv(f)
			if rec := e.post(runID, memberID, "Bearer good", body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status = %d, want 400", name, body, rec.Code)
			}
			if len(f.replaced) != 1 || f.replaced[0].Reason != unusable+want {
				t.Errorf("%s: replaced = %+v; want %q", name, f.replaced, want)
			}
		}
		f := newFakeMembers()
		set(&f.run)
		if rec := newMemberEnv(f).post(runID, memberID, "Bearer good", completedJSON("security")); rec.Code != http.StatusOK || len(f.replaced) != 0 {
			t.Errorf("%s: the right skills = %d, replaced %+v; want 200", name, rec.Code, f.replaced)
		}
	}
}

// A member ID under another run's path, or an unknown one, is the same 401
// as a bad token: nothing is stored, and the answer never tells whether the
// member exists.
func TestMemberHandlerWrongRun(t *testing.T) {
	cases := []struct{ run, member string }{
		{otherRun, memberID},
		{runID, "44444444-4444-4444-4444-444444444444"},
		{runID, "55555555-5555-5555-5555-555555555555"},
		{"not-a-uuid", memberID},
	}
	bad := newMemberEnv(newFakeMembers()).post(runID, memberID, "Bearer bad", completedJSON("security"))
	for _, c := range cases {
		e := newMemberEnv(newFakeMembers())
		rec := e.post(c.run, c.member, "Bearer good", completedJSON("security"))
		if rec.Code != http.StatusUnauthorized || rec.Body.String() != bad.Body.String() {
			t.Errorf("%s/%s: %d %q; want the bad token's %d %q", c.run, c.member, rec.Code, rec.Body, bad.Code, bad.Body)
		}
		if len(e.f.stored)+len(e.f.replaced)+len(e.f.reported) != 0 {
			t.Errorf("%s/%s: stored=%v replaced=%v reported=%v", c.run, c.member, e.f.stored, e.f.replaced, e.f.reported)
		}
	}
}

// Not JSON with a good token: the member fails at delivery.
func TestMemberHandlerInvalidJSON(t *testing.T) {
	f := newFakeMembers()
	f.storeErr = store.ErrInvalidResult
	e := newMemberEnv(f)
	rec := e.post(runID, memberID, "Bearer good", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(f.reported) != 1 || f.reported[0].Reason != unusable+"the body is not valid JSON" || f.reported[0].Step != "delivery" {
		t.Errorf("reported = %+v", f.reported)
	}
	if strings.Contains(e.logs.String(), "not json") {
		t.Errorf("the body reached the log:\n%s", e.logs.String())
	}
}

// The outcomes map to the run handler's answers.
func TestMemberHandlerOutcomes(t *testing.T) {
	for outcome, want := range map[store.ResultOutcome]int{
		store.ResultConflict:  http.StatusConflict,
		store.ResultDiscarded: http.StatusOK,
	} {
		f := newFakeMembers()
		f.outcome = outcome
		e := newMemberEnv(f)
		if rec := e.post(runID, memberID, "Bearer good", completedJSON("security")); rec.Code != want {
			t.Errorf("outcome %d: status = %d, want %d", outcome, rec.Code, want)
		}
		if len(f.replaced)+len(f.reported) != 0 {
			t.Errorf("outcome %d: replaced=%v reported=%v", outcome, f.replaced, f.reported)
		}
	}
	f := newFakeMembers()
	f.storeErr = errors.New("db down")
	if rec := newMemberEnv(f).post(runID, memberID, "Bearer good", completedJSON("security")); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: status = %d, want 500", rec.Code)
	}
}

// The body is capped as the run's is; a GET is 405.
func TestMemberHandlerLimits(t *testing.T) {
	e := newMemberEnv(newFakeMembers())
	big := `{"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	if rec := e.post(runID, memberID, "Bearer good", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: status = %d, want 413", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/members/"+memberID+"/result", nil)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
	if len(e.f.stored) != 0 {
		t.Errorf("stored = %v", e.f.stored)
	}
}

// The run stopped waiting right after the result was stored (a push took
// it): the result is left for the sweep, and the answer is 200.
func TestMemberHandlerRunGone(t *testing.T) {
	f := newFakeMembers()
	f.waits = false
	e := newMemberEnv(f)
	if rec := e.post(runID, memberID, "Bearer good", completedJSON("security")); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(f.replaced)+len(f.reported) != 0 {
		t.Errorf("replaced=%v reported=%v", f.replaced, f.reported)
	}
}

// The run's checks, moved into checkResult, give the same reasons.
func TestCheckResultUnchanged(t *testing.T) {
	run := store.Run{BaseSHA: base, HeadSHA: head, SkillSubset: []string{"security"}}
	for body, want := range map[string]string{
		completedJSON("security"):          "",
		resultJSON(base, "x"):              "the result is for a different commit than the one this run reviews",
		completedJSON("security", "style"): "the result has skills the Job was not started with",
		completedJSON("style"):             `the result lacks the skills the Job was started with ("security")`,
	} {
		res := parseOrFail(t, body)
		if got := checkResult(res, run); got != want {
			t.Errorf("checkResult(%s) = %q, want %q", body, got, want)
		}
	}
	// A whole review: the skills recorded at dispatch (or at the split).
	whole := store.Run{BaseSHA: base, HeadSHA: head, SkillSources: map[string]string{"security": "library"}}
	if got := checkResult(parseOrFail(t, completedJSON("security", "style")), whole); got != "the result has skills the Job was not started with" {
		t.Errorf("whole review, extra skill: %q", got)
	}
	merged := parseOrFail(t, completedJSON("security"))
	merged.Merged = true
	if got := checkResult(merged, run); got != "the result carries merge fields, which only pergent.sh sets" {
		t.Errorf("merged: %q", got)
	}
}

func parseOrFail(t *testing.T, body string) *result.Result {
	t.Helper()
	res, err := result.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return res
}
