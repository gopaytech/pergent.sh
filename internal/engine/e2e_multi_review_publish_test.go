package engine

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/gitlab"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/policy"
	"github.com/gopaytech/pergent.sh/internal/quota"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// editResult is body with edit applied to its JSON object.
func editResult(t *testing.T, body []byte, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// firstSkill is the first skill of a result's JSON object.
func firstSkill(m map[string]any) map[string]any { return m["skills"].([]any)[0].(map[string]any) }

// withMultiReview is body as the CLI writes a judge's result of n
// reviewers: the top multi_review block (every reviewer usable, judged) and
// each skill's block, with the reviewers in failed left out of its
// reviewers. A body that already has the block, or is not completed, is
// left as it is.
func withMultiReview(t *testing.T, body []byte, n int, failed ...int) []byte {
	t.Helper()
	return editResult(t, body, func(m map[string]any) {
		if _, ok := m["multi_review"]; ok || m["status"] != "completed" {
			return
		}
		var reviewers []any
		used := []any{}
		for k := 1; k <= n; k++ {
			reviewers = append(reviewers, map[string]any{"number": k, "usable": true})
			bad := false
			for _, f := range failed {
				bad = bad || f == k
			}
			if !bad {
				used = append(used, k)
			}
		}
		m["multi_review"] = map[string]any{"reviewers": reviewers, "judge": map[string]any{"judged": true}}
		for _, s := range m["skills"].([]any) {
			block := map[string]any{"reviewers": used, "judge": "ok"}
			if len(failed) > 0 {
				block["failed"] = failed
			}
			s.(map[string]any)["multi_review"] = block
		}
	})
}

// Only the judge run publishes, once: one comment whose header names the 3
// reviewers and the judge by their entry names and no provider or model,
// the success status of a clean merge, and the approval of the judge's run.
// A second pass posts nothing more.
func TestPublishMultiReviewOnce(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	env.GitLab.Reset()
	e.judgeAndPublish(t, id, withMultiReview(t, clean(t, head), 3))

	if n := len(env.GitLab.Requests(http.MethodPost, notesPath(1))); n != 1 {
		t.Fatalf("%d comments; want 1", n)
	}
	comment := createdComment(t, env, 1)
	if !strings.Contains(comment, "Multi review: team-a, team-b, team-c · judge team-j") {
		t.Errorf("comment lacks the multi review header:\n%s", comment)
	}
	for _, bad := range []string{"openai", "gpt-team-", "multi review off", "was skipped"} {
		if strings.Contains(comment, bad) {
			t.Errorf("comment holds %q:\n%s", bad, comment)
		}
	}
	if got := onlyStatus(t, env, head)["state"]; got != string(gitlab.StatusSuccess) {
		t.Errorf("status = %v; want success", got)
	}
	if n := len(env.GitLab.Requests(http.MethodPost, approvePath(1))); n != 1 || recordedApproval(t, env, 1) != head {
		t.Errorf("%d approvals, recorded %q; want the judge run's one", n, recordedApproval(t, env, 1))
	}
	env.GitLab.Reset()
	publish(t, env)
	for _, r := range env.GitLab.Requests("", "") {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s after the publish; want nothing", r.Method, r.Path)
		}
	}
}

// A judge result with no multi_review block does not say how it merged the
// reviewers: it is published as failed, never success, with no approval.
func TestPublishNoMultiReviewBlockFails(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, _ := releasedReview(t, e, head)
	dispatch(t, env)
	judgeJob(t, env, id)
	if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head)); code != http.StatusOK {
		t.Fatalf("the judge's result: status %d\n%s", code, env.Logs())
	}
	publish(t, env)
	wantFailedNeverPass(t, env, id, head)
	if comment := createdComment(t, env, 1); !strings.Contains(comment, reasonNoMultiReview) {
		t.Errorf("comment lacks why the result can't be used:\n%s", comment)
	}
}

// Reviewer 2 was skipped at the split on the token quota (its entry at its
// model limit): Skipped holds it with the fixed words, and the comment
// shows render's skip line once.
func TestPublishSkippedLine(t *testing.T) {
	e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
	env := e.env
	env.SetNow(novemberMid)
	b := globalRef(t, env, "gpt-b", "key-b")
	saveExtraReviewers(t, env, [][]store.EntryRef{{b}, {entryNamed(t, env, "team-c")}}, []store.EntryRef{entryNamed(t, env, "team-j")})
	seedEntryUsage(t, env, b, 10)
	setLimit(t, env, b, 0, 10)
	head := sha('a')
	id := splitReview(t, env, 1, head)
	two := memberOf(t, env, id, 2)
	wantSkipped(t, two, skipQuota)
	dispatch(t, env)
	releaseReviewers(t, e, id, []store.ReviewMember{memberOf(t, env, id, 1), memberOf(t, env, id, 3)}, clean(t, head), clean(t, head))

	rc, err := env.Engine.reviewComment(env.ctx, mustJudgeRun(t, e, id))
	if err != nil {
		t.Fatal(err)
	}
	name := two.Chain.Entries[0].Name
	if rc.names == nil || len(rc.names.Skipped) != 1 || rc.names.Skipped[0] != (render.SkippedReviewer{Name: name, Reason: "the token quota"}) {
		t.Fatalf("names = %+v; want reviewer 2 skipped on the token quota", rc.names)
	}
	if got := rc.names.Reviewers; len(got) != 3 || got[0] != "team-a" || got[1] != name || got[2] != "team-c" {
		t.Errorf("reviewers = %v; want team-a, %s, team-c", got, name)
	}
	e.postJudge(t, id, withMultiReview(t, clean(t, head), 3, 2))
	publish(t, env)
	wantPublished(t, env, id)
	comment := createdComment(t, env, 1)
	if n := strings.Count(comment, "Multi review: "+name+" was skipped: the token quota."); n != 1 {
		t.Errorf("the skip line is in the comment %d times; want once:\n%s", n, comment)
	}
}

// mustJudgeRun dispatches the released run id to its judge Job and returns
// the run as the publish reads it.
func mustJudgeRun(t *testing.T, e *e2e, id string) store.Run {
	t.Helper()
	dispatch(t, e.env)
	judgeJob(t, e.env, id)
	return e.env.Run(id)
}

// A reviewer with no result is named in fixed words only: a skip reason or
// why the provider refused its model as they are; any other reason (which
// can hold model text) as "its review failed".
func TestSkippedWords(t *testing.T) {
	for in, want := range map[string]string{
		skipQuota:                            skipQuota,
		skipSame:                             skipSame,
		skipNone:                             skipNone,
		"out of quota":                       "out of quota",
		reasonMemberRefused + "key rejected": "key rejected",
		"the model gpt-x said no":            skipReviewFailed,
		"":                                   skipReviewFailed,
	} {
		if got := skippedWords(in); got != want {
			t.Errorf("skippedWords(%q) = %q; want %q", in, got, want)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// A review whose multi review did not start says why in its footer, read
// from review_members (Ruling P42): the review image does not support it,
// or every extra reviewer is at its limit. An off row that an earlier,
// failed launch left (the reviewers removed before the retry) is not this
// review's: the comment is today's.
func TestPublishOffLine(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, env *testEnv)
		stale bool
		want  string
	}{
		"image": {setup: func(t *testing.T, env *testEnv) {
			multiReviewFloor(t, "")
			saveReviewers(t, env, [][]string{{"team-b"}}, []string{"team-j"})
		}, want: "the review image does not support it"},
		"every extra reviewer at its limit": {setup: func(t *testing.T, env *testEnv) {
			env.SetNow(novemberMid)
			b := globalRef(t, env, "gpt-b", "key-b")
			saveExtraReviewers(t, env, [][]store.EntryRef{{b}}, []store.EntryRef{entryNamed(t, env, "team-j")})
			seedEntryUsage(t, env, b, 10)
			setLimit(t, env, b, 0, 10)
		}, want: offQuota},
		"stale": {setup: func(t *testing.T, env *testEnv) {
			multiReviewFloor(t, "")
			saveReviewers(t, env, [][]string{{"team-b"}}, []string{"team-j"})
		}, stale: true, want: "the review image does not support it"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newMultiReviewE2E(t, []string{"team-a"}, nil, nil)
			env := e.env
			tc.setup(t, env)
			head := sha('a')
			servePublish(env, 1, head)
			id := env.Enqueue(1, head)
			if tc.stale {
				fails := 0
				env.Kube.PrependReactor("create", "jobs", func(act k8stesting.Action) (bool, runtime.Object, error) {
					if act.(k8stesting.CreateAction).GetObject().(*batchv1.Job).Name == kube.JobName(id) && fails == 0 {
						fails++
						return true, nil, errorString("the API server is down")
					}
					return false, nil, nil
				})
				dispatch(t, env)
				if env.Job(id) != nil || fails != 1 {
					t.Fatalf("the first launch did not fail\n%s", env.Logs())
				}
				saveExtraReviewers(t, env, nil, nil)
				env.SetNow(env.Now().Add(time.Hour))
			}
			dispatch(t, env)
			if env.Job(id) == nil {
				t.Fatalf("no Job\n%s", env.Logs())
			}
			if ms := reviewMembers(t, env, id); len(ms) != 1 || ms[0].State != store.MemberOff {
				t.Fatalf("members = %+v; want the off row", ms)
			}
			if code := e.postResult(t, id, string(env.Secret(id).Data["result-token"]), clean(t, head)); code != http.StatusOK {
				t.Fatalf("result: status %d", code)
			}
			publish(t, env)
			wantPublished(t, env, id)
			comment := createdComment(t, env, 1)
			line := policy.MultiReviewOffLine(tc.want)
			if got := strings.Contains(comment, line); got == tc.stale {
				t.Errorf("comment holds %q = %v; want %v:\n%s", line, got, !tc.stale, comment)
			}
			if strings.Contains(comment, "Multi review:") {
				t.Errorf("a single review's comment names a multi review:\n%s", comment)
			}
			if got := statusesOf(t, env, head); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "success") {
				t.Errorf("statuses = %v; want success last: the line changes the comment only", got)
			}
		})
	}
}

// Each Job's entry has text no comment may hold: its provider, model and
// base URL.
var hiddenModels = []struct{ name, provider, model, url string }{
	{"rev-one", "alpha-prov", "alpha-mdl", "https://llm-1.example.test/v1"},
	{"rev-two", "beta-prov", "beta-mdl", "https://llm-2.example.test/v1"},
	{"rev-three", "gamma-prov", "gamma-mdl", "https://llm-3.example.test/v1"},
	{"the-judge", "delta-prov", "delta-mdl", "https://llm-j.example.test/v1"},
	{"rev-three-b", "epsilon-prov", "epsilon-mdl", "https://llm-3b.example.test/v1"},
}

// newHiddenE2E is a multi review whose entries are hiddenModels, as team
// entries of a team that owns acme/widgets (key "key-" + name): chain A of
// the names one, reviewers 2.. of extra and the judge of judge.
func newHiddenE2E(t *testing.T, one []string, extra [][]string, judge []string) *e2e {
	t.Helper()
	multiReviewFloor(t, "v0.5.0")
	e := newE2E(t)
	env := e.env
	env.Engine.ScanImage = testScanImage
	team := teamOwning(t, env, "Payments")
	refs := map[string]store.EntryRef{}
	for _, h := range hiddenModels {
		m, err := env.Store.CreateTeamModel(env.ctx, store.TeamModel{TeamID: team, Name: h.name, Provider: h.provider,
			Model: h.model, BaseURL: h.url, APIKeySealed: env.Box.Seal([]byte("key-" + h.name))}, env.Now())
		if err != nil {
			t.Fatal(err)
		}
		refs[h.name] = store.EntryRef{Source: store.SourceTeam, ID: m.ID}
	}
	if err := env.Engine.Instances.LoadLLM(env.ctx); err != nil {
		t.Fatal(err)
	}
	of := func(names []string) []store.EntryRef {
		out := []store.EntryRef{}
		for _, n := range names {
			out = append(out, refs[n])
		}
		return out
	}
	saveChain(t, env, store.Chain{Scope: store.ScopeTeam, TeamID: team, Entries: of(one)})
	var chains [][]store.EntryRef
	for _, names := range extra {
		chains = append(chains, of(names))
	}
	saveExtraReviewers(t, env, chains, of(judge))
	return e
}

// newHiddenMultiReviewE2E is newHiddenE2E with 3 reviewers and a judge,
// each on its own entry of hiddenModels.
func newHiddenMultiReviewE2E(t *testing.T) *e2e {
	t.Helper()
	return newHiddenE2E(t, []string{"rev-one"}, [][]string{{"rev-two"}, {"rev-three"}}, []string{"the-judge"})
}

// fourJobs are the entries of newHiddenMultiReviewE2E.
var fourJobs = []string{"rev-one", "rev-two", "rev-three", "the-judge"}

// wantNoModelText checks text names no provider, model or base URL of the
// multi review's Jobs.
func wantNoModelText(t *testing.T, text string) {
	t.Helper()
	for _, h := range hiddenModels {
		for _, bad := range []string{h.provider, h.model, strings.TrimPrefix(h.url, "https://")} {
			if strings.Contains(text, bad) {
				t.Errorf("the text names %q:\n%s", bad, text)
			}
		}
	}
}

// modelText is text that names the provider, model and base URL of each
// entry of hiddenModels named in names.
func modelText(names ...string) string {
	var parts []string
	for _, h := range hiddenModels {
		if slices.Contains(names, h.name) {
			parts = append(parts, h.provider+"/"+h.model+" at "+h.url)
		}
	}
	return strings.Join(parts, ", ")
}

// Each reviewer's and the judge's provider, model and base URL, in a
// finding's text and the judge's reason, are hidden in the comment and in
// the finding's thread; the stored result keeps the full text.
func TestCommentHidesEveryModel(t *testing.T) {
	t.Run("comment", func(t *testing.T) {
		e := newHiddenMultiReviewE2E(t)
		env := e.env
		head := sha('a')
		id := splitReview(t, env, 1, head)
		dispatch(t, env)
		var ms []store.ReviewMember
		for n := 1; n <= 3; n++ {
			ms = append(ms, memberOf(t, env, id, n))
		}
		releaseReviewers(t, e, id, ms, clean(t, head), clean(t, head), clean(t, head))
		env.GitLab.Reset()
		text := modelText(fourJobs...)
		body := editResult(t, withMultiReview(t, clean(t, head), 3), func(m map[string]any) {
			s := firstSkill(m)
			s["findings"] = []any{map[string]any{"id": "abc123", "fingerprint": "abc123def456", "file": "a.go", "line": 1,
				"severity": "suggestion", "title": "Slow call to " + text, "explanation": "The call to " + text + " is slow.",
				"suggested_fix": "Ask " + text + " less.",
				"multi_review":  map[string]any{"found_by": []any{2}, "judge_disagreed": true, "reason": text + " thinks it is fine"}}}
		})
		e.judgeAndPublish(t, id, body)

		comment := createdComment(t, env, 1)
		wantNoModelText(t, comment)
		for _, want := range []string{"Multi review: rev-one, rev-two, rev-three · judge the-judge", "found by rev-two", "the model"} {
			if !strings.Contains(comment, want) {
				t.Errorf("comment lacks %q:\n%s", want, comment)
			}
		}
		if r := env.Run(id); !strings.Contains(string(r.Result), "delta-mdl") || !strings.Contains(string(r.Result), "beta-mdl") {
			t.Error("the stored result lost the models' names")
		}
	})
	t.Run("thread", func(t *testing.T) {
		var refs []policy.ModelRef
		for _, h := range hiddenModels {
			refs = append(refs, policy.ModelRef{Provider: h.provider, Model: h.model})
		}
		for _, multi := range []bool{false, true} {
			h := newHarness(t)
			b, _ := json.Marshal(map[string]any{"id": fpBlocking[:6], "fingerprint": fpBlocking, "file": "db/query.go", "line": 11,
				"severity": "blocking", "title": "Calls " + modelText(fourJobs...), "explanation": "Ask " + modelText(fourJobs...) + " less."})
			o := securityReview(t, h.head, "has_blocking_issues", b)
			o.Hide = func(s string) string { return policy.HideModels(s, refs, nil) }
			if multi {
				o.MultiReview = &render.MultiReviewNames{Reviewers: []string{"rev-one", "rev-two", "rev-three"}, Judge: "the-judge"}
			}
			h.sync(o)
			if len(h.fi.posted) == 0 || len(h.fi.posted[0]) == 0 {
				t.Fatalf("multi %v: no thread posted", multi)
			}
			body := h.fi.posted[0][0].Body
			named := false
			for _, m := range hiddenModels {
				named = named || strings.Contains(body, m.model) || strings.Contains(body, m.provider)
			}
			if named == multi {
				t.Errorf("multi %v: thread names a model = %v:\n%s", multi, named, body)
			}
		}
	})
}

// waitUsage waits for the usage writes the callbacks start after they
// answer.
func waitUsage(env *testEnv) { env.background.Wait() }

// The quota counts every Job: run_usage of the review holds the reviewers'
// and the judge's tokens per skill, run_usage_by_model has one reviewer row
// per reviewer and one judge row, quota.Check sees the total, and each Job
// counts on its own entry (Ruling L8).
func TestE2EMultiReviewUsage(t *testing.T) {
	e := newThreeReviewers(t)
	env := e.env
	head := sha('a')
	id, ms := startReviewers(t, e, head)
	releaseReviewers(t, e, id, ms, withTokens(t, clean(t, head), 100, 1), withTokens(t, clean(t, head), 200, 2),
		withTokens(t, clean(t, head), 300, 3))
	waitUsage(env)
	e.judgeAndPublish(t, id, withMultiReview(t, withTokens(t, clean(t, head), 30, 4), 3))
	waitUsage(env)

	if in, out, found, err := env.Store.RunUsage(env.ctx, id); err != nil || !found || in != 630 || out != 10 {
		t.Errorf("RunUsage = %d in, %d out, %v, %v; want 630 and 10: every reviewer and the judge", in, out, found, err)
	}
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
	var reviewers, judges int
	var judgeIn int64
	if err := conn.QueryRow(env.ctx, `SELECT count(*) FILTER (WHERE part = 'reviewer'), count(*) FILTER (WHERE part = 'judge'),
			COALESCE(sum(tokens_in) FILTER (WHERE part = 'judge'), 0)
		FROM run_usage_by_model WHERE run_id = $1::uuid`, id).Scan(&reviewers, &judges, &judgeIn); err != nil {
		t.Fatal(err)
	}
	if reviewers != 3 || judges != 1 || judgeIn != 30 {
		t.Errorf("run_usage_by_model = %d reviewer and %d judge rows (judge %d in); want 3, 1 and 30", reviewers, judges, judgeIn)
	}
	for limit, want := range map[int64]bool{640: true, 641: false} {
		used, _, err := quota.Check(env.ctx, env.Store, env.Now(), testInstance, testProjectID, settings.Effective{MonthlyTokenQuota: &limit})
		if err != nil || used != want {
			t.Errorf("quota of %d used up = %v, %v; want %v", limit, used, err, want)
		}
	}
	since := env.Now().Add(-24 * time.Hour)
	got, err := env.Store.EntryTokensSince(env.ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int64{}
	for _, name := range []string{"team-a", "team-b", "team-c", "team-j"} {
		ref := entryNamed(t, env, name)
		for _, row := range got {
			if row.Ref == ref {
				byName[name] += row.Tokens
			}
		}
	}
	for name, want := range map[string]int64{"team-a": 101, "team-b": 202, "team-c": 303, "team-j": 34} {
		if byName[name] != want {
			t.Errorf("entry %s used %d tokens; want %d (each Job on its own entry): %+v", name, byName[name], want, got)
		}
	}
}

// Reviewer 2 failed with an error that names its provider, model and base
// URL, and the judge's result repeats them; reviewers 1 and 3 are done.
// Through the judge and through --no-judge, the comment names none of them,
// and reviewer 2 is skipped in fixed words.
func TestPublishHidesFailedReviewerModel(t *testing.T) {
	for _, noJudge := range []bool{false, true} {
		t.Run(map[bool]string{false: "judge", true: "no judge"}[noJudge], func(t *testing.T) {
			e := newHiddenMultiReviewE2E(t)
			env := e.env
			if noJudge {
				execSQL(t, env, `DELETE FROM reviewer_chain_entries WHERE chain_id IN (SELECT id FROM reviewer_chains WHERE slot = 'judge')`)
			}
			head := sha('a')
			id := splitReview(t, env, 1, head)
			dispatch(t, env)
			var ms []store.ReviewMember
			for n := 1; n <= 3; n++ {
				ms = append(ms, memberOf(t, env, id, n))
			}
			two := hiddenModels[1]
			broke := "the call to " + two.provider + "/" + two.model + " at " + two.url + " broke"
			releaseReviewers(t, e, id, ms, clean(t, head), errorResult(t, head, broke), clean(t, head))
			dispatch(t, env)
			judgeJob(t, env, id)
			if noJudge {
				wantNoJudge(t, env, id, noJudgeChain)
			}
			env.GitLab.Reset()
			body := editResult(t, withMultiReview(t, clean(t, head), 3, 2), func(m map[string]any) {
				firstSkill(m)["failures"] = []any{map[string]any{"files": []any{"a.go"}, "message": "reviewer 2: " + broke}}
			})
			e.postJudge(t, id, body)
			publish(t, env)
			wantPublished(t, env, id)
			comment := createdComment(t, env, 1)
			wantNoModelText(t, comment)
			if !strings.Contains(comment, "Multi review: rev-two was skipped: "+skipReviewFailed+".") {
				t.Errorf("comment lacks reviewer 2's skip line in fixed words:\n%s", comment)
			}
		})
	}
}
