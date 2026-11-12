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
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gopaytech/pergent.sh/internal/config"
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

// fateAfter is the fate of the finding at db/query.go:12 that the security
// skill reports in the first review, through fatesInput and fates.Decide. Each
// argument is the security skill in one review, in order, or nil for a review
// that has another skill only. Every comparison edits line 12.
func fateAfter(t *testing.T, skills ...*result.Skill) string {
	t.Helper()
	var runs []store.Run
	for i, s := range skills {
		head := sha(byte('a' + i))
		if s == nil {
			other := fateSkill(t, "no_blocking_issues")
			other.Name = "style"
			runs = append(runs, store.Run{HeadSHA: head, Result: resultFor(t, head, other)})
			continue
		}
		runs = append(runs, store.Run{HeadSHA: head, Result: resultFor(t, head, *s)})
	}
	in := fatesInput(runs, nil)
	in.Compare = func(from, to string) ([]diffmap.File, error) {
		return []diffmap.File{diffmap.Parse("db/query.go", "", "@@ -11,3 +11,3 @@\n a\n-b\n+c\n d\n", false)}, nil
	}
	got, err := fates.Decide(in)
	if err != nil || len(got) != 1 {
		t.Fatalf("Decide = %+v, %v; want one fate", got, err)
	}
	return got[0].Fate
}

// withNotes is a security skill with verdict and left-out notes.
func withNotes(t *testing.T, verdict string, notes ...string) *result.Skill {
	t.Helper()
	s := fateSkill(t, verdict)
	s.Notes = notes
	return &s
}

// A note written by a review that failed says nothing the next review can
// lean on: the failed skill was skipped for the finding, and the note that the
// next review keeps is still the first it shows.
func TestFatesInputLeftOutNoteOfAFailedSkillIsStillNew(t *testing.T) {
	note := result.LeftOutNotePrefix + " Token compared with == (in api/auth.go)"
	more := result.LeftOutNotePrefix + " Another title (in api/user.go)"
	tests := []struct {
		name       string
		firstNotes []string
		then       []*result.Skill
		want       string
	}{
		{"kept after a failed review", nil,
			[]*result.Skill{withNotes(t, "failed", note), withNotes(t, "no_blocking_issues", note)}, fates.WentAway},
		{"rewritten in full after a failed review", nil,
			[]*result.Skill{withNotes(t, "failed", note), withNotes(t, "no_blocking_issues", more, note)}, fates.WentAway},
		// The note was written by the first review, a review without the skill
		// came between, and the next one keeps the note: it is not new.
		{"a review without the skill in between", []string{note},
			[]*result.Skill{nil, withNotes(t, "no_blocking_issues", note)}, fates.Fixed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := withNotes(t, "has_blocking_issues", tt.firstNotes...)
			b, err := json.Marshal(fateFinding("aaaaaaaaaaaa", "blocking", 12))
			if err != nil {
				t.Fatal(err)
			}
			first.Findings = append(first.Findings, b)
			if got := fateAfter(t, append([]*result.Skill{first}, tt.then...)...); got != tt.want {
				t.Errorf("fate = %s, want %s", got, tt.want)
			}
		})
	}
}

// leftOutThenGone is two reviews of one change, a to b, of the security skill:
// the first reports a finding at db/query.go:12 and carries the notes, the
// second reports nothing and carries secondNotes. The change from a to b edits
// line 12. It returns what the fate of the finding is.
func leftOutThenGone(t *testing.T, firstNotes, secondNotes []string) string {
	t.Helper()
	first := withNotes(t, "has_blocking_issues", firstNotes...)
	b, err := json.Marshal(fateFinding("aaaaaaaaaaaa", "blocking", 12))
	if err != nil {
		t.Fatal(err)
	}
	first.Findings = append(first.Findings, b)
	return fateAfter(t, first, withNotes(t, "no_blocking_issues", secondNotes...))
}

func TestFatesInputLeftOutNoteIsNewOnlyInTheReviewThatWroteIt(t *testing.T) {
	entry := func(titles ...string) string {
		return result.LeftOutNotePrefix + " " + strings.Join(titles, "; ")
	}
	kept := entry("Token compared with == (in api/auth.go)")
	both := entry("Token compared with == (in api/auth.go)", "SQL built by hand (in db/query.go)")
	fresh := entry("Another title (in api/user.go)")
	tests := []struct {
		name          string
		first, second []string
		want          string
	}{
		// A merge writes the new run's notes first and the kept ones after
		// them, and a kept note says nothing about the new review: the fix is
		// a fix.
		{"the note is kept by the next review", []string{kept}, []string{kept}, fates.Fixed},
		{"the next review writes another note", []string{kept}, []string{fresh, kept}, fates.WentAway},
		{"only the last note differs", []string{kept, both}, []string{kept, entry("Another title (in api/user.go)")}, fates.WentAway},
		{"the next review has a note and the first none", nil, []string{kept}, fates.WentAway},
		{"no note anywhere", nil, nil, fates.Fixed},
		// A rewrite that says no more than what was there is not new.
		{"a rewrite saying less than the kept note", []string{both}, []string{kept}, fates.Fixed},
		{"a rewrite with the entries in another order", []string{both}, []string{entry("SQL built by hand (in db/query.go)", "Token compared with == (in api/auth.go)")}, fates.Fixed},
		{"a rewrite with one more entry", []string{kept}, []string{both}, fates.WentAway},
		{"a rewrite whose list was cut", []string{both}, []string{kept + "; and 3 more"}, fates.Fixed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := leftOutThenGone(t, tt.first, tt.second); got != tt.want {
				t.Errorf("fate = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFatesInputLeftOutNoteIsComparedPerSkill(t *testing.T) {
	note := result.LeftOutNotePrefix + " Token compared with == (in api/auth.go)"
	first := fateSkill(t, "has_blocking_issues")
	first.Notes = []string{note}
	second := fateSkill(t, "has_blocking_issues")
	second.Notes = []string{note}
	other := fateSkill(t, "has_blocking_issues") // the same note on another skill is new there
	other.Name, other.Notes = "style", []string{note}
	got := fatesInput([]store.Run{
		{HeadSHA: sha('a'), Result: resultFor(t, sha('a'), first)},
		{HeadSHA: sha('b'), Result: resultFor(t, sha('b'), second, other)},
	}, nil)

	skills := got.Reviews[1].Skills
	if !got.Reviews[0].Skills[0].LeftOut || skills[0].LeftOut || !skills[1].LeftOut {
		t.Errorf("LeftOut = %v, %v, %v; want true, false, true", got.Reviews[0].Skills[0].LeftOut, skills[0].LeftOut, skills[1].LeftOut)
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

// A reviewed deleted file reaches fates as deleted: a finding on its removed
// lines that the next review, still deleting it, does not report again is
// fixed. A skill with no files.deleted has no Deleted, as before.
func TestFatesInputDeletedFiles(t *testing.T) {
	deletedSkill := func(findings ...result.Finding) result.Skill {
		s := fateSkill(t, "no_blocking_issues", findings...)
		s.Files.Reviewed = append(s.Files.Reviewed, "db/old.go")
		s.Files.Deleted = []string{"db/old.go"}
		return s
	}
	f := fateFinding("aaaaaaaaaaaa", "blocking", 5)
	f.File = "db/old.go"
	runs := []store.Run{
		{HeadSHA: sha('a'), Result: resultFor(t, sha('a'), deletedSkill(f))},
		{HeadSHA: sha('b'), Result: resultFor(t, sha('b'), deletedSkill())},
		{HeadSHA: sha('c'), Result: resultFor(t, sha('c'), fateSkill(t, "no_blocking_issues"))},
	}
	in := fatesInput(runs, nil)

	if d := in.Reviews[0].Skills[0].Deleted; !reflect.DeepEqual(d, map[string]bool{"db/old.go": true}) {
		t.Errorf("Deleted = %v; want db/old.go", d)
	}
	if d := in.Reviews[2].Skills[0].Deleted; d != nil {
		t.Errorf("Deleted = %v; want none", d)
	}

	// The file is deleted at both heads, so their diff doesn't touch it.
	in.Reviews = in.Reviews[:2]
	in.Compare = func(from, to string) ([]diffmap.File, error) {
		return []diffmap.File{diffmap.Parse("db/query.go", "", "@@ -11,3 +11,3 @@\n a\n-b\n+c\n d\n", false)}, nil
	}
	got, err := fates.Decide(in)
	if err != nil || len(got) != 1 || got[0].Fate != fates.Fixed {
		t.Errorf("Decide = %+v, %v; want one fixed fate", got, err)
	}
}

// ---------------------------------------------------------------------------
// The pure decisions: no database

func TestFateRetryDelay(t *testing.T) {
	for failed, want := range map[int]time.Duration{
		0: 5 * time.Minute, 1: 5 * time.Minute, 2: 10 * time.Minute, 3: 20 * time.Minute, 4: 40 * time.Minute, 5: 80 * time.Minute,
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
	calls []compareCall
}

type compareCall struct {
	projectID int64
	from, to  string
}

func (c *compareForge) CompareDiff(_ context.Context, projectID int64, from, to string) ([]forge.FileDiff, error) {
	c.calls = append(c.calls, compareCall{projectID, from, to})
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
	if wantCalls := []compareCall{{7, sha('a'), sha('b')}}; !reflect.DeepEqual(cf.calls, wantCalls) {
		t.Errorf("CompareDiff calls = %+v; want %+v", cf.calls, wantCalls)
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
		{"a reply too large on GitHub", fmt.Errorf("compare: %w", github.ErrTooLarge), true},
		{"a reply too large on GitLab", fmt.Errorf("compare: %w", gitlab.ErrTooLarge), true},
		// GitHub's 422 may be a head that is not there yet: try again.
		{"a 422 on GitHub", fmt.Errorf("compare: %w", &github.APIError{Status: http.StatusUnprocessableEntity}), false},
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

// botForge is a forge whose IsBot answers as set and counts its calls.
type botForge struct {
	forge.Forge
	bot   bool
	err   error
	calls int
}

func (b *botForge) IsBot(context.Context, int64) (bool, error) {
	b.calls++
	return b.bot, b.err
}

func TestFateAuthorIsBot(t *testing.T) {
	boom := errors.New("403")
	for _, tc := range []struct {
		name      string
		change    forge.Change
		forge     botForge
		want      bool
		untold    bool // IsBot failed, and the change counts as a person's
		wantErr   bool
		wantCalls int
	}{
		{name: "the platform said so (GitHub)", change: forge.Change{AuthorIsBot: true, AuthorID: 5}, want: true},
		{name: "GitHub person: IsBot is false", change: forge.Change{AuthorID: 5}, wantCalls: 1},
		{name: "GitLab bot", change: forge.Change{AuthorID: 5}, forge: botForge{bot: true}, want: true, wantCalls: 1},
		{name: "GitLab person", change: forge.Change{AuthorID: 5}, wantCalls: 1},
		{name: "can't be told: a person's", change: forge.Change{AuthorID: 5}, forge: botForge{err: boom}, untold: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.forge
			got, untold, err := fateAuthorIsBot(context.Background(), &f, tc.change)
			if got != tc.want || (untold != nil) != tc.untold || (err != nil) != tc.wantErr || f.calls != tc.wantCalls {
				t.Errorf("fateAuthorIsBot = %v, %v, %v with %d IsBot calls; want %v, untold %v, error %v, %d calls",
					got, untold, err, f.calls, tc.want, tc.untold, tc.wantErr, tc.wantCalls)
			}
		})
	}
}

// With the budget spent, IsBot's failure is not "a person's": merged fates
// are final, so the job is tried again.
func TestFateAuthorIsBotDeadline(t *testing.T) {
	bctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &botForge{err: context.DeadlineExceeded}
	if _, _, err := fateAuthorIsBot(bctx, f, forge.Change{AuthorID: 5}); err == nil {
		t.Error("an IsBot failure after the budget ran out was taken for a person's")
	}
	// The cause is flattened by some clients: it is the budget that counts.
	f = &botForge{err: errors.New("request failed")}
	if _, _, err := fateAuthorIsBot(bctx, f, forge.Change{AuthorID: 5}); err == nil {
		t.Error("a flattened IsBot failure after the budget ran out was taken for a person's")
	}
}

func TestFateTimeLeft(t *testing.T) {
	defer func(d time.Duration) { fateMinLeft = d }(fateMinLeft)
	fateMinLeft = time.Hour
	short, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if fateTimeLeft(short) {
		t.Error("a minute left counts as enough when an hour is the least")
	}
	long, cancel2 := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel2()
	if !fateTimeLeft(long) {
		t.Error("two hours left count as too little")
	}
	done, cancel3 := context.WithCancel(long)
	cancel3()
	if fateTimeLeft(done) {
		t.Error("a cancelled context has time left")
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

// warmFates makes the first query of a pass against a connection that is
// open, so the tests with a short budget don't spend it connecting. The
// budgets are also long enough that a slow database can't fail them: the hung
// fake holds until the budget ends, whatever it is.
func warmFates(t *testing.T, env *testEnv) {
	t.Helper()
	if _, err := env.Store.ClaimFateJobs(env.ctx, env.Now(), 1); err != nil {
		t.Fatal(err)
	}
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
	// One compare, from the first head to the second, straight.
	if reqs := env.GitLab.Requests(http.MethodGet, comparePath()); len(reqs) != 1 {
		t.Errorf("%d compares; want 1", len(reqs))
	} else if q := reqs[0].Query; q.Get("from") != sha('a') || q.Get("to") != sha('c') || q.Get("straight") != "true" {
		t.Errorf("compare query = %v; want from %s to %s, straight", q, sha('a'), sha('c'))
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
	if !j.Found || j.Finished || j.Attempts != 1 || j.NotBefore == nil || !j.NotBefore.Equal(start.Add(5*time.Minute)) {
		t.Errorf("job = %+v; want unfinished, 1 attempt, held until %s", j, start.Add(5*time.Minute))
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none", got)
	}

	// Held back for five minutes.
	env.GitLab.Reset()
	workFates(t, env)
	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("the job was tried again at once: %d calls", len(reqs))
	}

	// Then it works, and the platform recovers.
	env.SetNow(start.Add(5 * time.Minute))
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

	wantWait := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute}
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
		// GitHub says it in the pull request; its IsBot is always false.
		{"a GitHub bot", func(env *testEnv) {
			env.Engine.Platforms.Set(testInstance, Platform{Kind: config.PlatformGitHub, Forge: &fakeChangeForge{
				change: forge.Change{Number: 1, State: forge.StateMerged, AuthorID: 5, AuthorIsBot: true}}})
		}, true},
		{"a GitHub person", func(env *testEnv) {
			env.Engine.Platforms.Set(testInstance, Platform{Kind: config.PlatformGitHub, Forge: &fakeChangeForge{
				change: forge.Change{Number: 1, State: forge.StateMerged, AuthorID: 5}}})
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
	shortBudget(t, 2*time.Second)

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
	warmFates(t, env)

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

// An open change is only marked as looked at.
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

// A repository not switched on (here, no project row) is marked as looked
// at, with no platform call.
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

// fakeChangeForge is a platform that needs no HTTP: GetChange answers as set,
// IsBot is false as on GitHub. waitForBudget makes GetChange answer only once
// the context given to it is done (a platform call that outlives the pass's
// budget); panics makes it panic.
type fakeChangeForge struct {
	forge.Forge
	change        forge.Change
	waitForBudget bool
	panics        bool
}

func (f *fakeChangeForge) GetChange(ctx context.Context, _, _ int64) (forge.Change, error) {
	if f.panics {
		panic("a bad change")
	}
	if f.waitForBudget {
		<-ctx.Done()
	}
	return f.change, nil
}

func (f *fakeChangeForge) IsBot(context.Context, int64) (bool, error) { return false, nil }

// shortBudget sets a pass budget of budget, with room for a job to start.
func shortBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	b, l := fateBudget, fateMinLeft
	t.Cleanup(func() { fateBudget, fateMinLeft = b, l })
	fateBudget, fateMinLeft = budget, 50*time.Millisecond
}

// merged fates are final: a budget spent during IsBot is not "a person's".
func TestWorkFatesDeadlineDuringIsBot(t *testing.T) {
	shortBudget(t, 2*time.Second)
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "merged", 55)
	env.GitLab.Handle(http.MethodGet, userPath(55), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	requestFates(t, env, 1, true)
	warmFates(t, env)

	workFates(t, env)

	if j := fateJobOf(t, env, 1); j.Attempts != 1 || j.Finished {
		t.Errorf("job = %+v; want 1 attempt, unfinished\n%s", j, env.Logs())
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none", got)
	}
}

// The store write has the engine's context, not the budget's: a platform
// call that ends after the budget doesn't lose the fates.
func TestWorkFatesStoresAfterTheBudgetRanOut(t *testing.T) {
	shortBudget(t, 2*time.Second)
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.Engine.Platforms.Set(testInstance, Platform{Kind: config.PlatformGitHub, Forge: &fakeChangeForge{
		change: forge.Change{Number: 1, State: forge.StateMerged, AuthorID: 5}, waitForBudget: true}})
	requestFates(t, env, 1, true)
	warmFates(t, env)

	workFates(t, env)

	if got := storedFates(t, env, 1); len(got) != 1 {
		t.Errorf("fates = %+v; want the finding stored\n%s", got, env.Logs())
	}
}

// A webhook that asks again during a pass is not a failure: nothing is
// stored for the old request and the new one is worked out next.
func TestWorkFatesRequestedAgainMeanwhile(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.reviewOf(t, 1, sha('c'))
	serveChange(env, 1, sha('c'), "merged", 55)
	env.GitLab.Reply(http.MethodGet, userPath(55), http.StatusOK, map[string]any{"bot": false})
	var once sync.Once
	env.GitLab.Handle(http.MethodGet, comparePath(), func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			if err := env.Store.RequestFates(env.ctx, env.MRKey(1), testProjectPath, true, env.Now().Add(time.Second)); err != nil {
				t.Error(err)
			}
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"diffs": []fileDiff{removesLine12}})
	})
	requestFates(t, env, 1, true)

	workFates(t, env)

	if j := fateJobOf(t, env, 1); !j.Found || j.Finished || j.Attempts != 0 {
		t.Errorf("job = %+v; want the new request pending, no failed attempt", j)
	}
	if got := storedFates(t, env, 1); len(got) != 0 {
		t.Errorf("fates = %+v; want none for the old request", got)
	}
	if strings.Contains(env.Logs(), "will try again") {
		t.Errorf("a request made again was counted as a failure\n%s", env.Logs())
	}

	workFates(t, env)
	if got := storedFates(t, env, 1); len(got) != 1 || got[0].Fate != "fixed" {
		t.Errorf("fates after the next pass = %+v; want the finding fixed\n%s", got, env.Logs())
	}
}

// A job isn't started with little of the budget left: it would lose an
// attempt through no fault of its own.
func TestWorkFatesStartsNothingWithLittleTimeLeft(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	requestFates(t, env, 1, true)
	b, l := fateBudget, fateMinLeft
	defer func() { fateBudget, fateMinLeft = b, l }()
	fateBudget, fateMinLeft = time.Second, 2*time.Second

	workFates(t, env)

	if reqs := env.GitLab.Requests("", ""); len(reqs) != 0 {
		t.Errorf("%d platform calls with less than the least time left", len(reqs))
	}
	if j := fateJobOf(t, env, 1); j.Attempts != 0 || j.Finished {
		t.Errorf("job = %+v; want it untouched", j)
	}
}

// An instance with no platform right now is no fault of the job: it waits a
// minute without losing an attempt.
func TestWorkFatesNoPlatformKeepsTheAttempts(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	requestFates(t, env, 1, true)
	env.Engine.Platforms.Delete(testInstance)

	workFates(t, env)

	j := fateJobOf(t, env, 1)
	if !j.Found || j.Finished || j.Attempts != 0 || j.NotBefore == nil || !j.NotBefore.Equal(env.Now().Add(time.Minute)) {
		t.Errorf("job = %+v; want unfinished, no attempt, held until %s", j, env.Now().Add(time.Minute))
	}
}

// One bad change can't take the loop, and the reviews it runs with, down.
func TestWorkFatesPanicIsAFailedAttempt(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	env.Engine.Platforms.Set(testInstance, Platform{Kind: config.PlatformGitHub, Forge: &fakeChangeForge{panics: true}})
	requestFates(t, env, 1, true)

	workFates(t, env)

	if j := fateJobOf(t, env, 1); j.Attempts != 1 || j.Finished {
		t.Errorf("job = %+v; want 1 attempt, unfinished", j)
	}
	if !strings.Contains(env.Logs(), "panicked") {
		t.Errorf("the panic was not logged\n%s", env.Logs())
	}
}

func TestFatesSweepFindsAClosedChange(t *testing.T) {
	env := newTestEnv(t)
	env.reviewOf(t, 1, sha('a'), fateFinding(fpFirst, "blocking", 12))
	serveChange(env, 1, sha('a'), "closed", 55)

	env.Engine.sweepFates(env.ctx)

	if j := fateJobOf(t, env, 1); !j.Found || j.Finished || j.Merged {
		t.Errorf("job = %+v; want a pending close\n%s", j, env.Logs())
	}
}

// The sweep reads the repository's current path from the project, not from the
// old run: a renamed repository isn't taken for one that is switched off.
func TestFatesSweepFollowsARenamedRepository(t *testing.T) {
	env := newTestEnv(t)
	decide(t, env, env.MRKey(1), "acme/old-name", sha('a'), resultFor(t, sha('a'), fateSkill(t, "no_blocking_issues")))
	serveChange(env, 1, sha('a'), "merged", 55)

	env.Engine.sweepFates(env.ctx)

	if j := fateJobOf(t, env, 1); !j.Found || !j.Merged {
		t.Errorf("job = %+v; want a pending merge\n%s", j, env.Logs())
	}
}

// ---------------------------------------------------------------------------
// The linked repositories' check: no database

// impactFate is the fate of the impact finding (render/comment.go:12) after
// the impact skills of the reviews, in order; a nil skill is a review that has
// only another skill. Every comparison edits line 12.
func impactFate(t *testing.T, skills ...*result.Skill) string {
	t.Helper()
	var runs []store.Run
	for i, s := range skills {
		head := sha(byte('a' + i))
		if s == nil {
			other := fateSkill(t, "no_blocking_issues")
			other.Name = "style"
			runs = append(runs, store.Run{HeadSHA: head, Result: resultFor(t, head, other)})
			continue
		}
		runs = append(runs, store.Run{HeadSHA: head, Result: resultFor(t, head, *s)})
	}
	in := fatesInput(runs, nil)
	in.Compare = func(from, to string) ([]diffmap.File, error) {
		return []diffmap.File{diffmap.Parse("render/comment.go", "", "@@ -11,3 +11,3 @@\n a\n-b\n+c\n d\n", false)}, nil
	}
	got, err := fates.Decide(in)
	if err != nil || len(got) != 1 {
		t.Fatalf("Decide = %+v, %v; want one fate", got, err)
	}
	if got[0].Skill != result.ImpactSkill {
		t.Fatalf("skill = %q; want impact", got[0].Skill)
	}
	return got[0].Fate
}

// The check lists no reviewed file, yet a finding it stops reporting is gone
// when the check finished, and stays open when it failed or did not run.
func TestImpactFindingsGetAFate(t *testing.T) {
	open := func() *result.Skill { s := impactSkillOf(t, "has_blocking_issues", "blocking"); return &s }
	clean := func() *result.Skill { s := impactSkillOf(t, "no_blocking_issues", ""); return &s }
	failed := func() *result.Skill { s := impactSkillOf(t, "failed", ""); return &s }

	if got := impactFate(t, open(), clean()); got != fates.Fixed {
		t.Errorf("after a clean check: fate = %s; want fixed (the lines changed)", got)
	}
	if got := impactFate(t, open(), clean(), clean()); got != fates.Fixed {
		t.Errorf("after two clean checks: fate = %s; want fixed", got)
	}
	if got := impactFate(t, open()); got != fates.Open {
		t.Errorf("reported to the end: fate = %s; want open", got)
	}
	if got := impactFate(t, open(), failed()); got != fates.Open {
		t.Errorf("after a failed check: fate = %s; want open", got)
	}
	if got := impactFate(t, open(), nil); got != fates.Open {
		t.Errorf("after a review where it did not run: fate = %s; want open", got)
	}
}

func TestFatesInputMarksOnlyImpactAsWhole(t *testing.T) {
	sec := fateSkill(t, "no_blocking_issues")
	imp := impactSkillOf(t, "no_blocking_issues", "")
	got := fatesInput([]store.Run{{HeadSHA: sha('a'), Result: resultFor(t, sha('a'), sec, imp)}}, nil)
	skills := got.Reviews[0].Skills
	if len(skills) != 2 || skills[0].Whole || !skills[1].Whole {
		t.Errorf("Whole = %+v; want only the impact skill", skills)
	}
}
