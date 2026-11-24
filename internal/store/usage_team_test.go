package store

import (
	"slices"
	"testing"
	"time"
)

// A team's usage counts the splitUsageSQL rows whose owner team is the team:
// a multi review once, each Job on its own model, the rows of a repository
// that moved staying with the team they had, and only from since on. The
// start of owner team usage is the oldest row with any owner team.
func TestTeamUsage(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, found, err := s.OwnerTeamUsageStart(ctx); err != nil || found {
		t.Fatalf("OwnerTeamUsageStart with no rows = %v, %v; want not found", found, err)
	}

	// Project 9 is never owned: its row has no owner team, and it is older
	// than every owned row.
	seedUsage(t, s, usageMR(9, 1), t0.Add(-72*time.Hour), TriggerPush, false, "m-none", skillRow("security", "no_blocking_issues", 1000, 0))
	a, b := newTeam(t, s, "Payments"), newTeam(t, s, "Widgets")
	// A owns acme/widgets (the multi review's project 7) and acme/p8; B owns acme/p10.
	if err := setOwner(t, s, a, TeamRepo{Instance: mr1.Instance, Project: "acme/widgets", Owner: true},
		TeamRepo{Instance: mr1.Instance, Project: "acme/p8", Owner: true}); err != nil {
		t.Fatal(err)
	}
	ownRepo(t, s, b, mr1.Instance, "acme/p10")
	oldest := t0.Add(-48 * time.Hour)
	seedUsage(t, s, usageMR(8, 1), oldest, TriggerPush, false, "m-a", skillRow("security", "no_blocking_issues", 500, 0))
	seedUsage(t, s, usageMR(8, 2), t0.Add(-20*time.Hour), TriggerPush, false, "m-a", skillRow("security", "no_blocking_issues", 7, 1))
	seedUsage(t, s, usageMR(10, 1), t0, TriggerPush, false, "m-b", skillRow("security", "no_blocking_issues", 300, 30))

	// A 3-reviewer multi review with a judge on A's acme/widgets.
	id, ms := multiReviewWithEntries(t, s, mr1, "a1", 3)
	for i, tokens := range []SkillUsage{{Skill: "security", In: 100, Out: 10}, {Skill: "security", In: 50, Out: 5}, {Skill: "security", In: 30, Out: 3}} {
		doneReviewer(t, s, ms[i].ID, `{"status":"completed"}`)
		if err := s.RecordReviewMemberUsage(ctx, ms[i].ID, []SkillUsage{tokens}); err != nil {
			t.Fatal(err)
		}
	}
	judgeRun(t, s, id, JudgeRow{Reviewers: []int{1, 2, 3}, Provider: "pj", Model: "judge-m",
		Entry: RunEntry{Source: SourceGlobal, ID: "jg", Name: "judge-main", Position: 1, Length: 1}})
	if err := s.RecordJudgeUsage(ctx, id, []SkillUsage{{Skill: "security", Verdict: "no_blocking_issues", In: 20, Out: 2}}); err != nil {
		t.Fatal(err)
	}

	// acme/p8 moves to B: its next row counts on B.
	if err := setOwner(t, s, a, TeamRepo{Instance: mr1.Instance, Project: "acme/widgets", Owner: true}); err != nil {
		t.Fatal(err)
	}
	if err := setOwner(t, s, b, TeamRepo{Instance: mr1.Instance, Project: "acme/p10", Owner: true},
		TeamRepo{Instance: mr1.Instance, Project: "acme/p8", Owner: true}); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, s, usageMR(8, 3), t0.Add(time.Hour), TriggerPush, false, "m-a", skillRow("security", "no_blocking_issues", 40, 4))

	since := t0.Add(-24 * time.Hour)
	usage := func(t *testing.T, team int64, since time.Time) TeamUsage {
		t.Helper()
		u, err := s.TeamUsage(ctx, team, since)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	type repo struct {
		project string
		tokens  int64
		reviews int64
	}
	reposOf := func(u TeamUsage) []repo {
		out := []repo{}
		for _, r := range u.Repos {
			if r.Instance != mr1.Instance || r.GlobalTokens != 0 || r.TeamTokens != 0 {
				t.Errorf("repo %+v; want instance %s and no source split", r, mr1.Instance)
			}
			out = append(out, repo{r.ProjectPath, r.Tokens.In + r.Tokens.Out, r.Reviews})
		}
		return out
	}

	t.Run("only the team's rows", func(t *testing.T) {
		u := usage(t, a, since)
		// The multi review (200 in, 20 out, 4 rows without cache counts) and
		// acme/p8's row in the window. Not B's, not the row with no owner.
		wantTokens(t, "A's total", u.Tokens, TokenSum{In: 207, Out: 21, Unknown: 4})
		if u.Reviews != 2 {
			t.Errorf("A's reviews = %d; want 2", u.Reviews)
		}
		if got := usage(t, b, since); got.Tokens.In != 340 || got.Tokens.Out != 34 || got.Reviews != 2 {
			t.Errorf("B's usage = %+v; want acme/p10 and acme/p8 after the move", got)
		}
	})

	t.Run("multi review once", func(t *testing.T) {
		u := usage(t, a, since)
		if got, want := reposOf(u), []repo{{"acme/widgets", 220, 1}, {"acme/p8", 8, 1}}; !slices.Equal(got, want) {
			t.Errorf("A's repositories = %+v; want %+v", got, want)
		}
		type model struct {
			provider, name string
			tokens, runs   int64
		}
		var got []model
		for _, m := range u.Models {
			got = append(got, model{m.Provider, m.Name, m.Tokens.In + m.Tokens.Out, m.Runs})
		}
		want := []model{{"p1", "m1", 110, 1}, {"p2", "m2", 55, 1}, {"p3", "m3", 33, 1}, {"pj", "judge-m", 22, 1}, {"", "m-a", 8, 1}}
		if !slices.Equal(got, want) {
			t.Errorf("A's models = %+v; want %+v", got, want)
		}
	})

	t.Run("moved repository", func(t *testing.T) {
		if got, want := reposOf(usage(t, b, since)), []repo{{"acme/p10", 330, 1}, {"acme/p8", 44, 1}}; !slices.Equal(got, want) {
			t.Errorf("B's repositories = %+v; want %+v", got, want)
		}
		// acme/p8's rows from before the move stay on A.
		if got := reposOf(usage(t, a, since)); len(got) != 2 || got[1] != (repo{"acme/p8", 8, 1}) {
			t.Errorf("A's repositories = %+v; want acme/p8's 8 tokens from before the move", got)
		}
	})

	t.Run("window", func(t *testing.T) {
		u := usage(t, a, since)
		if len(u.Daily) != 2 {
			t.Fatalf("A's daily = %+v; want 2 days", u.Daily)
		}
		jan1, jan2 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		if !u.Daily[0].Day.Equal(jan1) || !u.Daily[1].Day.Equal(jan2) || u.Daily[0].Day.Location() != time.UTC {
			t.Errorf("days = %v, %v; want midnight UTC of the 1st and the 2nd", u.Daily[0].Day, u.Daily[1].Day)
		}
		wantTokens(t, "the 1st", u.Daily[0].Tokens, TokenSum{In: 7, Out: 1})
		wantTokens(t, "the 2nd", u.Daily[1].Tokens, TokenSum{In: 200, Out: 20, Unknown: 4})
		// From further back, the older row of acme/p8 counts too.
		if got := usage(t, a, oldest); got.Tokens.In != 707 || got.Reviews != 3 || len(got.Daily) != 3 {
			t.Errorf("A from %v = %+v; want the older row too", oldest, got)
		}
	})

	t.Run("start", func(t *testing.T) {
		start, found, err := s.OwnerTeamUsageStart(ctx)
		if err != nil || !found || !start.Equal(oldest) {
			t.Errorf("OwnerTeamUsageStart = %v, %v, %v; want %v, the oldest owned row, not the older row with no owner", start, found, err, oldest)
		}
	})
}
