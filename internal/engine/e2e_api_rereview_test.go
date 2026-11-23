package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/reviewapi"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const dismissReason = "Checked with the gateway team: 5 is intended."

// dismiss sets finding of review id aside through the API.
func (a *apiE2E) dismiss(t *testing.T, id, finding, reason string) {
	t.Helper()
	body, _ := json.Marshal(reviewapi.DismissRequest{Reason: reason})
	req, _ := http.NewRequest(http.MethodPost, a.api.URL+"/v1/reviews/"+id+"/findings/"+finding+"/dismiss", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.key)
	resp, err := a.api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dismiss %s of %s: status %d", finding, id, resp.StatusCode)
	}
}

// finish gives review id, dispatched, its result with skills and publishes it.
func (a *apiE2E) finish(t *testing.T, id string, skills ...result.Skill) {
	t.Helper()
	im, err := reviewapi.BuildImport(twoFiles())
	if err != nil {
		t.Fatal(err)
	}
	token := string(a.env.Secret(id).Data["result-token"])
	if code := a.postResult(t, id, token, apiResultFor(t, im.Base, im.Head, skills...)); code != http.StatusOK {
		t.Fatalf("result of %s: status %d", id, code)
	}
	publish(t, a.env)
	wantPublished(t, a.env, id)
}

// reReview reviews route:1 once, dismisses its blocking finding, and asks for
// the subject's second review, dispatched. It returns both IDs.
func (a *apiE2E) reReview(t *testing.T) (first, second string) {
	t.Helper()
	env := a.env
	first = a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	a.finish(t, first, blockingSkill(t))
	body := a.stored(t, first)
	if len(body.Skills) != 1 || len(body.Skills[0].Findings) != 1 {
		t.Fatalf("first review = %+v, want its one finding", body)
	}
	a.dismiss(t, first, body.Skills[0].Findings[0].ID, dismissReason)

	second = a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	if env.Job(second) == nil {
		t.Fatalf("no Job for the second review; run = %+v\n%s", env.Run(second), env.Logs())
	}
	return first, second
}

// A later review of a subject is given its newest completed review and what
// people dismissed on the subject, as a merge request's re-review is.
func TestAPIReReviewGetsPreviousAndDismissals(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	_, second := a.reReview(t)

	data := env.ConfigMap(second).Data
	var previous struct {
		Status string `json:"status"`
		Skills []struct {
			Name     string           `json:"name"`
			Findings []result.Finding `json:"findings"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(data["previous.json"]), &previous); err != nil {
		t.Fatalf("previous.json = %q: %v", data["previous.json"], err)
	}
	if previous.Status != "completed" || len(previous.Skills) != 1 || previous.Skills[0].Name != "security" ||
		len(previous.Skills[0].Findings) != 1 || previous.Skills[0].Findings[0].Fingerprint != sqlFingerprint {
		t.Errorf("previous.json = %s; want the first review's finding", data["previous.json"])
	}
	var past map[string][]actions.PastDismissal
	if err := json.Unmarshal([]byte(data["dismissals.json"]), &past); err != nil {
		t.Fatalf("dismissals.json = %q: %v", data["dismissals.json"], err)
	}
	want := []actions.PastDismissal{{File: "routes/api.yaml", Title: "The limit is too low", Severity: "blocking", Reason: dismissReason}}
	if len(past) != 1 || !slices.Equal(past["security"], want) {
		t.Errorf("dismissals.json = %s; want %+v under security", data["dismissals.json"], want)
	}
	if v := pergentEnv(env, second)["PERGENT_DISMISSALS"]; v != "/in/dismissals.json" {
		t.Errorf("PERGENT_DISMISSALS = %q, want /in/dismissals.json", v)
	}
	if args := env.Job(second).Spec.Template.Spec.Containers[0].Args; !slices.Contains(args, "--previous=/in/previous.json") {
		t.Errorf("the Job's arguments %v have no --previous", args)
	}
	if !strings.Contains(env.Logs(), "past dismissals") {
		t.Errorf("the dismissals are not logged:\n%s", env.Logs())
	}
	noGitLab(t, env)
}

// The first review of a subject has neither.
func TestAPIFirstReviewHasNoPrevious(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	id := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	data := env.ConfigMap(id).Data
	if _, ok := data["previous.json"]; ok {
		t.Errorf("a first review has previous.json: %s", data["previous.json"])
	}
	if _, ok := data["dismissals.json"]; ok {
		t.Errorf("a first review has dismissals.json: %s", data["dismissals.json"])
	}
	if _, ok := pergentEnv(env, id)["PERGENT_DISMISSALS"]; ok {
		t.Error("a first review has PERGENT_DISMISSALS")
	}
}

// A finding a later review repeats is marked dismissed, and no longer
// blocks.
func TestAPIReReviewMarksDismissed(t *testing.T) {
	a := newAPIE2E(t)
	env := a.env
	_, second := a.reReview(t)

	a.finish(t, second, blockingSkill(t))

	body := a.stored(t, second)
	if body.Status != reviewapi.StatusCompleted || body.Verdict != reviewapi.VerdictNoBlockingIssues {
		t.Errorf("second review = %s, %s; want completed, no_blocking_issues", body.Status, body.Verdict)
	}
	if len(body.Skills) != 1 || len(body.Skills[0].Findings) != 1 {
		t.Fatalf("second review's skills = %+v, want the one finding, kept", body.Skills)
	}
	s := body.Skills[0]
	if s.Verdict != reviewapi.VerdictNoBlockingIssues {
		t.Errorf("skill verdict = %s, want no_blocking_issues", s.Verdict)
	}
	if d := s.Findings[0].Dismissed; d == nil || d.Reason != dismissReason || d.At.IsZero() {
		t.Errorf("finding dismissed = %+v; want the reason %q and a time", d, dismissReason)
	}
	if r := env.Run(second); r.Outcome != store.OutcomeNoBlocking {
		t.Errorf("outcome = %q, want no blocking", r.Outcome)
	}
	if got := a.review(t, second); got["verdict"] != reviewapi.VerdictNoBlockingIssues {
		t.Errorf("GET verdict = %v, want no_blocking_issues", got["verdict"])
	}
}

// A review queued before its client's budget was used up fails at the start,
// with no Job, and its callback says why.
func TestBudgetUsedUpAtDispatch(t *testing.T) {
	rc := newReceiver(t, nil)
	a := newCallbackE2E(t, rc.srv.URL+"/hook")
	env := a.env
	first := a.ask(t, "route:1", twoFiles())
	dispatch(t, env)
	a.finish(t, first, blockingSkill(t))
	// What the result handler records too, in the background; the row is
	// kept once, so the sum is the same whichever lands first.
	if err := env.Store.RecordUsage(env.ctx, first, []store.SkillUsage{{Skill: "security", Verdict: "has_blocking_issues", In: 1200, Out: 300}}); err != nil {
		t.Fatal(err)
	}

	second := a.ask(t, "route:2", twoFiles())
	if _, _, err := env.Store.UpdateAPIClient(env.ctx, "gateway", func(c *store.APIClient) { c.Limits.MonthlyTokenBudget = 1500 }); err != nil {
		t.Fatal(err)
	}
	dispatch(t, env)
	if env.Job(second) != nil {
		t.Fatalf("a review past its client's budget got a Job")
	}
	publish(t, env)

	a.wantFailed(t, second, reviewapi.StepStarting, reasonBudget)
	if reasonBudget != reviewapi.BudgetUsedUpMessage {
		t.Errorf("the failure %q and the 429 %q say it differently", reasonBudget, reviewapi.BudgetUsedUpMessage)
	}

	catchUp(env)
	deliver(t, env)
	var got *reviewapi.Review
	for _, req := range rc.requests() {
		var b reviewapi.Review
		if err := json.Unmarshal(req.body, &b); err != nil {
			t.Fatalf("callback body %q: %v", req.body, err)
		}
		if b.ID == second {
			got = &b
		}
	}
	if got == nil {
		t.Fatalf("no callback for the review past the budget; got %d", len(rc.requests()))
	}
	if got.Status != reviewapi.StatusFailed || got.Failure == nil || got.Failure.Reason != reasonBudget || got.Failure.Step != reviewapi.StepStarting {
		t.Errorf("callback = %+v; want failed at starting: %s", got, reasonBudget)
	}
	noGitLab(t, env)
}
