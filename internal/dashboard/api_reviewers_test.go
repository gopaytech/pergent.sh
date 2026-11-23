package dashboard

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	reviewerFloor      = "v9.0.0"
	reviewerImageOK    = "ghcr.io/x/pergent:v9.0.0"
	msgNoReviewerYet   = "no review image supports multi review yet"
	msgReviewerNeeds   = "a second reviewer needs a review image with pergent v9.0.0 or later; jobs.image is "
	warnNoJudge        = "No judge: findings are merged without checking"
	reviewersProject   = "acme/gadgets"
	reviewersAuditName = "reviewers_changed"
)

// setReviewerFloor sets the first pergent with multi review and the review
// image, the floor going back to its default after the test.
func setReviewerFloor(t *testing.T, e *env, floor, image string) {
	t.Helper()
	old := config.PanelMinVersion
	config.PanelMinVersion = floor
	t.Cleanup(func() { config.PanelMinVersion = old })
	e.cfg.Jobs.Image = image
}

// gadgetsOwnedByPayments adds main/acme/gadgets as a dashboard repository
// that Payments owns.
func (te *teamsEnv) gadgetsOwnedByPayments() {
	te.t.Helper()
	te.dashboardRepo("main", "acme/gadgets", nil)
	te.editTeam(te.payments, func(tm *store.Team) {
		tm.Repos = append(tm.Repos, store.TeamRepo{Instance: "main", Project: "acme/gadgets", Owner: true})
	})
}

func reviewersURL(project string) string { return "/api/repositories/main/" + project + "/reviewers" }

// reviewersEnv is a teamsEnv with main/acme/gadgets, a dashboard repository
// that Payments owns, and these entries: one (the default, so reviewer 1),
// shared (teams may use it), private (teams may not), and Payments' own and
// own2.
type reviewersEnv struct {
	*teamsEnv
	one, shared, private store.LLMModel
	own, own2            TeamModelView
}

func newReviewersEnv(t *testing.T) *reviewersEnv {
	t.Helper()
	re := &reviewersEnv{teamsEnv: newTeamsEnv(t)}
	re.gadgetsOwnedByPayments()
	re.one = re.addModel("openai", "gpt-one", "", llmKeyThree)
	re.shared, re.private = re.sharedAndPrivate()
	re.own = re.addTeamModel(re.maintainer, re.payments, "own", teamKeyOne)
	re.own2 = re.addTeamModel(re.maintainer, re.payments, "own2", teamKeyTwo)
	return re
}

// globalRef and teamRef are an entry's "source/id".
func globalRef(m store.LLMModel) string { return "global/" + m.ID }
func teamRef(m TeamModelView) string    { return "team/" + m.ID }

// reviewersBody is a PUT .../reviewers body: judge, then reviewers 2.. as
// "source/id" lists.
func reviewersBody(judge []string, reviewers ...[]string) map[string]any {
	rs := make([]any, 0, len(reviewers))
	for _, r := range reviewers {
		rs = append(rs, entryBody(r...)["entries"])
	}
	return map[string]any{"reviewers": rs, "judge": entryBody(judge...)["entries"]}
}

// refsOfTexts are the refs of "source/id" texts.
func refsOfTexts(texts ...string) []store.EntryRef {
	out := make([]store.EntryRef, 0, len(texts))
	for _, s := range texts {
		v := entryBody(s)["entries"].([]map[string]string)[0]
		out = append(out, store.EntryRef{Source: v["source"], ID: v["id"]})
	}
	return out
}

// storeReviewers saves the repository's reviewers directly, past every
// check of the API.
func (re *reviewersEnv) storeReviewers(byAdmin bool, judge []string, extra ...[]string) {
	re.t.Helper()
	s := store.ReviewersSave{Instance: "main", Project: reviewersProject, Judge: refsOfTexts(judge...), SavedByAdmin: byAdmin}
	for _, e := range extra {
		s.Extra = append(s.Extra, refsOfTexts(e...))
	}
	if _, err := re.st.SaveReviewers(ctx, s, re.clock.now()); err != nil {
		re.t.Fatal(err)
	}
}

// stored is what the store holds: each extra reviewer's refs as
// "source/id", and the judge's.
func (re *reviewersEnv) stored() (extra [][]string, judge []string, judgeFound bool) {
	re.t.Helper()
	r, err := re.st.Reviewers(ctx, "main", reviewersProject)
	if err != nil {
		re.t.Fatal(err)
	}
	texts := func(rs []store.EntryRef) []string {
		out := []string{}
		for _, ref := range rs {
			out = append(out, ref.Source+"/"+ref.ID)
		}
		return out
	}
	for _, c := range r.Extra {
		extra = append(extra, texts(c.Entries))
	}
	return extra, texts(r.Judge.Entries), r.JudgeFound
}

func (re *reviewersEnv) wantNothingStored(what string) {
	re.t.Helper()
	if extra, judge, found := re.stored(); len(extra) != 0 || len(judge) != 0 || found {
		re.t.Errorf("%s: stored %v, judge %v (%v)", what, extra, judge, found)
	}
	if got := auditDetails(re.env, reviewersAuditName); len(got) != 0 {
		re.t.Errorf("%s: %s = %q", what, reviewersAuditName, got)
	}
}

// numbersAndRefs are v's reviewers as "<n>: source/id ..." lines.
func numbersAndRefs(v ReviewersView) []string {
	out := []string{}
	for _, r := range v.Reviewers {
		line := strconv.Itoa(r.Number) + ":"
		for _, ref := range chainRefs(r.Chain) {
			line += " " + ref
		}
		out = append(out, line)
	}
	return out
}

// With the floor unset, adding reviewer 2 is refused and nothing is stored.
func TestReviewersRefusedWithoutFloor(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, "", reviewerImageOK)
	rec := re.sendJSON(re.admin, "PUT", reviewersURL(reviewersProject), reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared)}))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNoReviewerYet {
		t.Errorf("PUT = %d %s", rec.Code, rec.Body)
	}
	re.wantNothingStored("after the refusal")
	if v := getJSON[ReviewersView](re.env, re.admin, reviewersURL(reviewersProject)); v.AddRefusal != msgNoReviewerYet {
		t.Errorf("addRefusal = %q", v.AddRefusal)
	}
}

// With an image at the floor an admin saves 3 extra reviewers and a judge;
// GET shows them, and each slot is audited once, by entry names only.
func TestReviewersSavedWithFloor(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	body := reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared), teamRef(re.own)}, []string{globalRef(re.private)}, []string{teamRef(re.own2)})
	v := decode[ReviewersView](t, re.sendJSON(re.admin, "PUT", reviewersURL(reviewersProject), body), "PUT", http.StatusOK)
	want := []string{"2: " + globalRef(re.shared) + " " + teamRef(re.own), "3: " + globalRef(re.private), "4: " + teamRef(re.own2)}
	if got := numbersAndRefs(v); !slices.Equal(got, want) || v.ReadOnly || v.AddRefusal != "" || len(v.Warnings) != 0 {
		t.Errorf("PUT = %+v (%q); want %q", v, got, want)
	}
	if v.Judge == nil || !slices.Equal(chainRefs(*v.Judge), []string{globalRef(re.shared)}) {
		t.Errorf("judge = %+v", v.Judge)
	}
	for _, r := range v.Reviewers {
		if r.Chain.Scope != store.ScopeRepo || r.Chain.Source != "repository" || !r.Chain.SavedByAdmin || r.Chain.ReadOnly || len(r.Chain.Choices) == 0 {
			t.Errorf("reviewer %d = %+v", r.Number, r.Chain)
		}
	}
	got := getJSON[ReviewersView](re.env, re.viewer, reviewersURL(reviewersProject))
	if !slices.Equal(numbersAndRefs(got), want) || !got.ReadOnly || got.Judge == nil || !got.Judge.ReadOnly {
		t.Errorf("GET as a viewer = %+v", got)
	}
	if got, want := auditDetails(re.env, reviewersAuditName), []string{
		"reviewer=2 entries=openai/gpt-shared, own",
		"reviewer=3 entries=openai/gpt-private",
		"reviewer=4 entries=own2",
		"judge entries=openai/gpt-shared",
	}; !slices.Equal(got, want) {
		t.Errorf("%s = %q; want %q", reviewersAuditName, got, want)
	}
	if got := auditDetails(re.env, "chain_changed"); len(got) != 0 {
		t.Errorf("chain_changed = %q; reviewer 1 did not change", got)
	}
}

// An image older than the floor is refused with its tag in the message.
func TestReviewersImageTooOld(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, "ghcr.io/x/pergent:v8.0.0")
	rec := re.sendJSON(re.admin, "PUT", reviewersURL(reviewersProject), reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared)}))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgReviewerNeeds+"v8.0.0" {
		t.Errorf("PUT = %d %s", rec.Code, rec.Body)
	}
	re.wantNothingStored("after the refusal")
}

// An image whose tag is not a version is refused, and so is an image with no
// tag, each with its own words.
func TestReviewersImageNotVersion(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/x/pergent:main": msgReviewerNeeds + "main (not a version)",
		"ghcr.io/x/pergent":      msgReviewerNeeds + "not a version",
	} {
		re := newReviewersEnv(t)
		setReviewerFloor(t, re.env, reviewerFloor, image)
		rec := re.sendJSON(re.admin, "PUT", reviewersURL(reviewersProject), reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared)}))
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != want {
			t.Errorf("image %s: PUT = %d %s; want 400 %q", image, rec.Code, rec.Body, want)
		}
		re.wantNothingStored("after the refusal")
	}
}

// Only a save that adds a reviewer needs the image: with the floor unset, a
// maintainer edits a stored reviewer and removes one.
func TestReviewersRemoveNeedsNoImage(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, "", reviewerImageOK)
	re.storeReviewers(true, []string{teamRef(re.own)}, []string{globalRef(re.shared)}, []string{teamRef(re.own2)})
	url := reviewersURL(reviewersProject)
	decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{teamRef(re.own)}, []string{teamRef(re.own2)}, []string{globalRef(re.shared)})),
		"edit", http.StatusOK)
	v := decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{teamRef(re.own)}, []string{teamRef(re.own2)})), "remove", http.StatusOK)
	if got := numbersAndRefs(v); !slices.Equal(got, []string{"2: " + teamRef(re.own2)}) {
		t.Errorf("after the remove = %q", got)
	}
	v = decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, reviewersBody(nil)), "remove all", http.StatusOK)
	if len(v.Reviewers) != 0 || v.Judge != nil {
		t.Errorf("after removing all = %+v", v)
	}
	if extra, _, found := re.stored(); len(extra) != 0 || found {
		t.Errorf("stored = %v, judge %v", extra, found)
	}
}

// [Access] The rights of chain A: a viewer reads and gets 403 on a write, a
// maintainer of another team that sees the repository gets 403, someone who
// does not see it 404, and a maintainer of the owner team saves.
func TestReviewersRights(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	url, body := reviewersURL(reviewersProject), reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.shared)})

	rec := re.sendJSON(re.viewer, "PUT", url, body)
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgRepoChainWrite {
		t.Errorf("PUT as a viewer = %d %s", rec.Code, rec.Body)
	}
	// Widgets covers acme/payments but does not own it.
	rec = re.sendJSON(re.otherMaintainer, "PUT", reviewersURL("acme/payments"), body)
	if rec.Code != http.StatusForbidden || errorOf(t, rec) != msgRepoChainWrite {
		t.Errorf("PUT as a maintainer of another team = %d %s", rec.Code, rec.Body)
	}
	// Widgets does not cover acme/gadgets.
	wantCode(t, re.get(re.otherMaintainer, url), "GET as a non-member", http.StatusNotFound)
	wantCode(t, re.sendJSON(re.otherMaintainer, "PUT", url, body), "PUT as a non-member", http.StatusNotFound)
	re.wantNothingStored("after the refused writes")

	v := decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, body), "PUT as the maintainer", http.StatusOK)
	if v.ReadOnly || len(v.Reviewers) != 1 || v.Reviewers[0].Chain.SavedByAdmin || v.Reviewers[0].Chain.ReadOnly {
		t.Errorf("PUT as the maintainer = %+v", v)
	}
	v = getJSON[ReviewersView](re.env, re.viewer, url)
	if !v.ReadOnly || len(v.Reviewers) != 1 || !v.Reviewers[0].Chain.ReadOnly || len(v.Reviewers[0].Chain.Choices) != 0 ||
		v.Judge == nil || !v.Judge.ReadOnly || len(v.Judge.Choices) != 0 {
		t.Errorf("GET as a viewer = %+v", v)
	}
}

// Another team's entry is refused with chain A's text, as a reviewer and as
// the judge.
func TestReviewersForeignEntry(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	foreign := re.addTeamModel(re.otherMaintainer, re.widgets, "theirs", teamKeyTwo)
	for name, body := range map[string]map[string]any{
		"reviewer": reviewersBody([]string{teamRef(re.own)}, []string{teamRef(foreign)}),
		"judge":    reviewersBody([]string{teamRef(foreign)}, []string{teamRef(re.own)}),
	} {
		rec := re.sendJSON(re.maintainer, "PUT", reviewersURL(reviewersProject), body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgForeignTeamEntry {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	re.wantNothingStored("after the refusals")
}

// Two reviewers may not start with the same model, whatever its case; a
// backup may repeat one.
func TestReviewersSameFirstModel(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	loud := decode[TeamModelView](t, re.sendJSON(re.maintainer, "POST", teamURL(re.payments, "/models"),
		map[string]any{"name": "loud", "provider": "OpenAI", "model": "GPT-One", "apiKey": teamKeyOne}), "POST team model", http.StatusCreated)
	url := reviewersURL(reviewersProject)
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"as reviewer 1": {reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.shared)}, []string{teamRef(loud)}),
			"reviewer 3 starts with the same model as reviewer 1"},
		"as reviewer 2": {reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.shared)}, []string{globalRef(re.shared), teamRef(re.own)}),
			"reviewer 3 starts with the same model as reviewer 2"},
	} {
		rec := re.sendJSON(re.admin, "PUT", url, tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want {
			t.Errorf("%s = %d %s; want %q", name, rec.Code, rec.Body, tc.want)
		}
	}
	re.wantNothingStored("after the refusals")
	decode[ReviewersView](t, re.sendJSON(re.admin, "PUT", url,
		reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.shared), teamRef(loud)}, []string{teamRef(re.own), globalRef(re.one), globalRef(re.shared)})), "backups", http.StatusOK)
}

// The counts and empty chains are refused with their own texts, before any
// entry is looked at.
func TestReviewersCountsAndEmpties(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	j := []string{globalRef(re.shared)}
	five := [][]string{{"global/a"}, {"global/b"}, {"global/c"}, {"global/d"}, {"global/e"}}
	for name, tc := range map[string]struct {
		body any
		want string
	}{
		"6 reviewers":       {reviewersBody(j, five...), "at most 5 reviewers"},
		"empty reviewer":    {reviewersBody(j, []string{globalRef(re.shared)}, []string{}), "a reviewer needs an entry; remove it instead"},
		"no judge":          {reviewersBody(nil, []string{globalRef(re.shared)}), "2 or more reviewers need a judge"},
		"judge alone":       {reviewersBody(j), "a judge needs 2 or more reviewers"},
		"reviewer too long": {reviewersBody(j, []string{"global/a", "global/b", "global/c", "global/d", "global/e", "global/f"}), msgChainTooLong},
		"judge too long":    {reviewersBody([]string{"global/a", "global/b", "global/c", "global/d", "global/e", "global/f"}, []string{globalRef(re.shared)}), msgChainTooLong},
		"entry twice":       {reviewersBody(j, []string{globalRef(re.shared), globalRef(re.shared)}), msgEntryTwice},
		"no reviewers key":  {map[string]any{"judge": []any{}}, msgBadReviewersBody},
		"unknown key":       {map[string]any{"reviewers": []any{}, "judge": []any{}, "x": 1}, msgBadReviewersBody},
		"bad source":        {reviewersBody(j, []string{"other/a"}), msgBadReviewersBody},
	} {
		rec := re.sendJSON(re.admin, "PUT", reviewersURL(reviewersProject), tc.body)
		if rec.Code != http.StatusBadRequest || errorOf(t, rec) != tc.want {
			t.Errorf("%s = %d %s; want %q", name, rec.Code, rec.Body, tc.want)
		}
	}
	re.wantNothingStored("after the refusals")
}

// The card warns when the judge chain lost its last entry outside the card,
// and when reviewer 1's chain now starts with reviewer 2's model.
func TestReviewersWarnings(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	url := reviewersURL(reviewersProject)
	decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{teamRef(re.own2)}, []string{teamRef(re.own)}, []string{globalRef(re.shared)})),
		"PUT", http.StatusOK)
	wantCode(t, re.send(re.maintainer, "DELETE", teamURL(re.payments, "/models/"+re.own2.ID), ""), "delete own2", http.StatusNoContent)
	v := getJSON[ReviewersView](re.env, re.viewer, url)
	if v.Judge == nil || len(v.Judge.Entries) != 0 || !slices.Equal(v.Warnings, []string{warnNoJudge}) || len(v.Reviewers) != 2 {
		t.Errorf("after the judge's entry went = %+v", v)
	}
	decode[ChainView](t, re.sendJSON(re.maintainer, "PUT", repoChainURL(reviewersProject), entryBody(teamRef(re.own))), "chain A", http.StatusOK)
	v = getJSON[ReviewersView](re.env, re.viewer, url)
	if want := []string{warnNoJudge, "Reviewer 2 starts with the same model as reviewer 1, so it runs on its next entry"}; !slices.Equal(v.Warnings, want) {
		t.Errorf("warnings = %q; want %q", v.Warnings, want)
	}
}

// A same first model that this save did not touch only warns: reviewer 3
// matches reviewer 1 after chain A changed, and a save of the judge alone
// passes. A save that changes reviewer 3 to another entry of that model is
// refused.
func TestReviewersSameFirstModelUnchangedPairWarns(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	url := reviewersURL(reviewersProject)
	re.storeReviewers(false, []string{teamRef(re.own2)}, []string{globalRef(re.shared)}, []string{teamRef(re.own)})
	decode[ChainView](t, re.sendJSON(re.maintainer, "PUT", repoChainURL(reviewersProject), entryBody(teamRef(re.own))), "chain A", http.StatusOK)
	warn := "Reviewer 3 starts with the same model as reviewer 1, so it runs on its next entry"

	v := decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url,
		reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared)}, []string{teamRef(re.own)})), "judge only", http.StatusOK)
	if !slices.Equal(v.Warnings, []string{warn}) || v.Judge == nil || !slices.Equal(chainRefs(*v.Judge), []string{globalRef(re.shared)}) {
		t.Errorf("after the judge save = %+v; want the warning %q", v, warn)
	}
	copyOfOwn := decode[TeamModelView](t, re.sendJSON(re.maintainer, "POST", teamURL(re.payments, "/models"),
		map[string]any{"name": "own-copy", "provider": "OpenAI", "model": "GPT-Own", "apiKey": teamKeyOne}), "POST team model", http.StatusCreated)
	rec := re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{globalRef(re.shared)}, []string{globalRef(re.shared)}, []string{teamRef(copyOfOwn)}))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != "reviewer 3 starts with the same model as reviewer 1" {
		t.Errorf("a changed reviewer 3 = %d %s", rec.Code, rec.Body)
	}
	if extra, _, _ := re.stored(); len(extra) != 2 || !slices.Equal(extra[1], []string{teamRef(re.own)}) {
		t.Errorf("stored = %v; want reviewer 3 unchanged", extra)
	}
}

// One audit entry per slot whose entries changed: removing reviewer 2 of 3
// moves reviewer 3 down and empties its slot; the judge did not change.
func TestReviewersAuditPerSlot(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	re.storeReviewers(false, []string{teamRef(re.own2)}, []string{globalRef(re.shared)}, []string{teamRef(re.own)})
	decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", reviewersURL(reviewersProject), reviewersBody([]string{teamRef(re.own2)}, []string{teamRef(re.own)})),
		"remove reviewer 2", http.StatusOK)
	if got, want := auditDetails(re.env, reviewersAuditName), []string{"reviewer=2 entries=own", "reviewer=3 entries=(none)"}; !slices.Equal(got, want) {
		t.Errorf("%s = %q; want %q", reviewersAuditName, got, want)
	}
	decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", reviewersURL(reviewersProject), reviewersBody(nil)), "remove all", http.StatusOK)
	if got, want := auditDetails(re.env, reviewersAuditName)[2:], []string{"reviewer=2 entries=(none)", "judge entries=(none)"}; !slices.Equal(got, want) {
		t.Errorf("%s = %q; want %q", reviewersAuditName, got, want)
	}
}

// A maintainer removes reviewer 2 in front of an admin's reviewer 3 that
// names a global entry teams may not use: the moved chain is not checked and
// keeps saved_by_admin. A changed chain with that entry is refused.
func TestReviewersMaintainerRemovesBeforeAdminReviewer(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	re.storeReviewers(true, []string{teamRef(re.own)}, []string{globalRef(re.shared)}, []string{globalRef(re.private)})
	url := reviewersURL(reviewersProject)
	rec := re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.shared)}, []string{globalRef(re.private), teamRef(re.own2)}))
	if rec.Code != http.StatusBadRequest || errorOf(t, rec) != msgNotTeamsMayUse {
		t.Errorf("a changed reviewer 3 = %d %s", rec.Code, rec.Body)
	}
	v := decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url, reviewersBody([]string{teamRef(re.own)}, []string{globalRef(re.private)})), "remove", http.StatusOK)
	if got := numbersAndRefs(v); !slices.Equal(got, []string{"2: " + globalRef(re.private)}) || !v.Reviewers[0].Chain.SavedByAdmin {
		t.Errorf("after the remove = %+v", v)
	}
	r, err := re.st.Reviewers(ctx, "main", reviewersProject)
	if err != nil || len(r.Extra) != 1 || !r.Extra[0].SavedByAdmin {
		t.Errorf("stored = %+v, %v; want reviewer 2 saved by an admin", r, err)
	}
}

// The repository page: on with the count and the judge's first entry name,
// off with none stored.
func TestRepositoryViewMultiReview(t *testing.T) {
	t.Parallel()
	re := newReviewersEnv(t)
	multi := func() map[string]any {
		t.Helper()
		return getJSON[struct {
			MultiReview map[string]any `json:"multiReview"`
		}](re.env, re.viewer, "/api/repositories/main/"+reviewersProject).MultiReview
	}
	want := func(what string, got, want map[string]any) {
		t.Helper()
		if !maps.Equal(got, want) {
			t.Errorf("%s: multiReview = %v; want %v", what, got, want)
		}
	}
	want("none", multi(), map[string]any{"on": false, "reviewers": float64(1), "judge": ""})
	re.storeReviewers(true, []string{teamRef(re.own), globalRef(re.shared)}, []string{globalRef(re.shared)}, []string{globalRef(re.private)})
	want("2 extra", multi(), map[string]any{"on": true, "reviewers": float64(3), "judge": "own"})
	wantCode(t, re.send(re.maintainer, "DELETE", teamURL(re.payments, "/models/"+re.own.ID), ""), "delete own", http.StatusNoContent)
	want("judge renumbered", multi(), map[string]any{"on": true, "reviewers": float64(3), "judge": "openai/gpt-shared"})
}

// No reviewers response, to anyone, holds a key; nor does the audit or the
// log.
func TestReviewersNoKeyInResponses(t *testing.T) {
	re := newReviewersEnv(t)
	setReviewerFloor(t, re.env, reviewerFloor, reviewerImageOK)
	url := reviewersURL(reviewersProject)
	decode[ReviewersView](t, re.sendJSON(re.admin, "PUT", url,
		reviewersBody([]string{globalRef(re.private)}, []string{globalRef(re.shared), teamRef(re.own)}, []string{globalRef(re.private)})), "PUT as the admin", http.StatusOK)
	decode[ReviewersView](t, re.sendJSON(re.maintainer, "PUT", url,
		reviewersBody([]string{globalRef(re.private)}, []string{teamRef(re.own2)}, []string{globalRef(re.private)})), "PUT as the maintainer", http.StatusOK)
	re.markEntry(store.EntryRef{Source: store.SourceTeam, ID: re.own2.ID}, store.ClassAuth, nil)
	until := re.clock.now().Add(time.Hour)
	re.markEntry(store.EntryRef{Source: store.SourceGlobal, ID: re.shared.ID}, store.ClassQuota, &until)
	for _, c := range []*http.Cookie{re.admin, re.maintainer, re.viewer} {
		re.get(c, url)
		re.get(c, "/api/repositories/main/"+reviewersProject)
	}
	re.get(re.admin, "/api/audit")
	re.wantNoSecret(llmKeyOne, llmKeyTwo, llmKeyThree, teamKeyOne, teamKeyTwo)
}

// The reviewer chains read the model and team limits as chain A does: an
// entry whose model limit is used up reads model_limit, and a team entry at
// the owner team's limit reads team_limit, in a reviewer and in the judge.
func TestReviewersShowLimitStates(t *testing.T) {
	t.Parallel()
	re := newReviewersEnv(t)
	re.storeReviewers(true, []string{teamRef(re.own)}, []string{globalRef(re.shared), teamRef(re.own)})
	used := re.usageRun(runFix{project: reviewersProject, number: 99, head: head('9')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	re.exec(`UPDATE runs SET model_source = 'global', model_entry_id = $2 WHERE id = $1::uuid`, used, re.shared.ID)
	used = re.usageRun(runFix{project: reviewersProject, number: 98, head: head('8')}, usageSkill("code-review", "no_blocking_issues", 6, 4))
	re.exec(`UPDATE runs SET model_source = 'team', model_entry_id = $2 WHERE id = $1::uuid`, used, re.own.ID)
	for _, l := range []store.ModelLimit{
		{Ref: store.EntryRef{Source: store.SourceGlobal, ID: re.shared.ID}, MonthlyTokens: 10},
		{Ref: store.EntryRef{Source: store.SourceTeam, ID: re.own.ID}, TeamID: re.payments, MonthlyTokens: 10},
	} {
		l.UpdatedAt, l.UpdatedBy = re.clock.now(), "test"
		if err := re.st.SetModelLimit(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	states := func(c ChainView) []string {
		out := []string{}
		for _, en := range c.Entries {
			out = append(out, en.State)
		}
		return out
	}
	v := getJSON[ReviewersView](re.env, re.viewer, reviewersURL(reviewersProject))
	if len(v.Reviewers) != 1 || !slices.Equal(states(v.Reviewers[0].Chain), []string{"model_limit", "team_limit"}) {
		t.Errorf("reviewers = %+v; want reviewer 2 with model_limit, team_limit", v.Reviewers)
	}
	if v.Judge == nil || !slices.Equal(states(*v.Judge), []string{"team_limit"}) {
		t.Errorf("judge = %+v; want team_limit", v.Judge)
	}
}
