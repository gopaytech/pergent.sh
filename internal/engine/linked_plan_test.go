package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gopaytech/pergent/render"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/forge/githubforge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/kube"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// These tests need no database: planLinks and the glue around it read the
// platform through two small functions.

const planSource = "acme/gadgets"

func lk(target string, canBlock bool, watched ...string) store.Link {
	return store.Link{Target: target, Watched: watched, CanBlock: canBlock}
}

// planWorld is a platform with visibilities and targets, counting its calls.
type planWorld struct {
	vis       map[string]forge.Visibility
	visErr    map[string]error
	targets   map[string]githubforge.Target
	targetErr map[string]error
	calls     int
}

func (w *planWorld) Visibility(_ context.Context, repo string) (forge.Visibility, error) {
	w.calls++
	if err := w.visErr[repo]; err != nil {
		return "", err
	}
	return w.vis[repo], nil
}

func (w *planWorld) Target(_ context.Context, repo string) (githubforge.Target, error) {
	w.calls++
	if err := w.targetErr[repo]; err != nil {
		return githubforge.Target{}, err
	}
	return w.targets[repo], nil
}

func newPlanWorld() *planWorld {
	return &planWorld{
		vis: map[string]forge.Visibility{
			planSource: forge.VisPrivate, "acme/api": forge.VisPrivate, "acme/web": forge.VisPublic,
			"acme/docs": forge.VisInternal,
		},
		visErr: map[string]error{}, targetErr: map[string]error{},
		targets: map[string]githubforge.Target{
			"acme/api":  {RepoID: 11, InstallationID: 55},
			"acme/web":  {RepoID: 12, InstallationID: 55},
			"acme/docs": {RepoID: 13, InstallationID: 55},
		},
	}
}

func (w *planWorld) input(ls []store.Link, files ...string) linkInput {
	return linkInput{
		Source: planSource, SourceInstallation: 55, RepoURL: "https://github.com/acme/gadgets.git",
		Links: ls, Files: files, Visibility: w.Visibility, Target: w.Target,
	}
}

func TestPlanLinksNothingMatched(t *testing.T) {
	w := newPlanWorld()
	plan := planLinks(context.Background(), w.input([]store.Link{lk("acme/api", true, "render/**")}, "cmd/main.go"))
	if len(plan.Entries) != 0 || len(plan.Clones) != 0 || len(plan.Matched) != 0 {
		t.Errorf("plan = %+v; want none", plan)
	}
	if b, _ := plan.encode(); b != nil {
		t.Errorf("encode = %s; want nil", b)
	}
	if w.calls != 0 {
		t.Errorf("%d platform calls for a change that matched nothing", w.calls)
	}
}

func TestPlanLinksClonesMatched(t *testing.T) {
	w := newPlanWorld()
	ls := []store.Link{lk("acme/api", false, "render/**"), lk("acme/web", true), lk("acme/docs", true, "docs/**")}
	plan := planLinks(context.Background(), w.input(ls, "render/comment.go"))

	want := []linkedEntry{
		{Name: "acme/api", Path: "/work/linked-1", Watched: []string{"render/**"}, CanBlock: false},
		{Name: "acme/web", Path: "/work/linked-2", Watched: []string{}, CanBlock: true},
	}
	if !slices.EqualFunc(plan.Entries, want, func(a, b linkedEntry) bool {
		return a.Name == b.Name && a.Path == b.Path && a.CanBlock == b.CanBlock && a.Error == b.Error && slices.Equal(a.Watched, b.Watched)
	}) {
		t.Errorf("entries = %+v; want %+v", plan.Entries, want)
	}
	wantClones := []kube.LinkedClone{
		{Dir: "linked-1", CloneURL: "https://github.com/acme/api.git"},
		{Dir: "linked-2", CloneURL: "https://github.com/acme/web.git"},
	}
	if !slices.Equal(plan.Clones, wantClones) {
		t.Errorf("clones = %+v", plan.Clones)
	}
	if !slices.Equal(plan.RepoIDs, []int64{11, 12}) {
		t.Errorf("repo IDs = %v", plan.RepoIDs)
	}
	if len(plan.Matched) != 2 || plan.Matched[0].Target != "acme/api" || plan.Matched[1].Target != "acme/web" {
		t.Errorf("matched = %+v", plan.Matched)
	}
	b, err := plan.encode()
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil || len(back) != 2 {
		t.Fatalf("encoded = %s, %v", b, err)
	}
	for _, k := range []string{"name", "path", "watched", "can_block", "error"} {
		if _, ok := back[1][k]; !ok {
			t.Errorf("entry has no %q: %s", k, b)
		}
	}
	// An empty list is [], never null.
	if !strings.Contains(string(b), `"watched":[]`) {
		t.Errorf("empty watched is not []: %s", b)
	}
	for _, c := range plan.Clones {
		if strings.Contains(c.CloneURL, "@") {
			t.Errorf("clone URL %q has userinfo", c.CloneURL)
		}
	}
}

// Check(source, target) takes two values of one type: a swap compiles. A
// private repository may link a public one; a public one may not link a
// private one. The two cases fail if the arguments are swapped.
func TestPlanLinksVisibilityDirection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		source, targ forge.Visibility
		wantErr      string
	}{
		{"private links public", forge.VisPrivate, forge.VisPublic, ""},
		{"private links internal", forge.VisPrivate, forge.VisInternal, ""},
		{"internal links public", forge.VisInternal, forge.VisPublic, ""},
		{"public links public", forge.VisPublic, forge.VisPublic, ""},
		{"public links private", forge.VisPublic, forge.VisPrivate, linkErrRefused},
		{"public links internal", forge.VisPublic, forge.VisInternal, linkErrRefused},
		{"internal links private", forge.VisInternal, forge.VisPrivate, linkErrRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld()
			w.vis[planSource], w.vis["acme/api"] = tc.source, tc.targ
			plan := planLinks(context.Background(), w.input([]store.Link{lk("acme/api", true)}, "a.go"))
			if len(plan.Entries) != 1 || plan.Entries[0].Error != tc.wantErr {
				t.Fatalf("entries = %+v; want error %q", plan.Entries, tc.wantErr)
			}
			if (tc.wantErr == "") != (len(plan.Clones) == 1) {
				t.Errorf("clones = %+v with error %q", plan.Clones, plan.Entries[0].Error)
			}
		})
	}
}

func TestPlanLinksErrorEntries(t *testing.T) {
	boom := errors.New("boom: ghs_secret-from-the-platform")
	for _, tc := range []struct {
		name string
		edit func(w *planWorld, in *linkInput)
		want string
	}{
		{"the target's visibility can't be read", func(w *planWorld, _ *linkInput) { w.visErr["acme/api"] = boom }, linkErrVisibility},
		{"the source's visibility can't be read", func(w *planWorld, _ *linkInput) { w.visErr[planSource] = boom }, linkErrVisibility},
		{"the source's visibility is unsupported", func(w *planWorld, _ *linkInput) { w.visErr[planSource] = forge.ErrUnsupported }, linkErrVisibility},
		{"the App can't read the target", func(w *planWorld, _ *linkInput) { w.visErr["acme/api"] = forge.ErrSourceNotFound }, linkErrNotReadable},
		{"an unknown visibility", func(w *planWorld, _ *linkInput) { w.vis["acme/api"] = "" }, linkErrRefused},
		{"the lookup fails", func(w *planWorld, _ *linkInput) { w.targetErr["acme/api"] = boom }, linkErrLookup},
		{"the lookup finds nothing", func(w *planWorld, _ *linkInput) { w.targetErr["acme/api"] = forge.ErrSourceNotFound }, linkErrNotReadable},
		{"another installation", func(w *planWorld, _ *linkInput) {
			w.targets["acme/api"] = githubforge.Target{RepoID: 11, InstallationID: 99}
		}, linkErrInstall},
		{"a clone URL that is not https", func(_ *planWorld, in *linkInput) { in.RepoURL = "http://github.com/acme/gadgets.git" }, linkErrURL},
		{"a clone URL with userinfo", func(_ *planWorld, in *linkInput) { in.RepoURL = "https://x:tok@github.com/acme/gadgets.git" }, linkErrURL},
		{"links that are not valid", func(_ *planWorld, in *linkInput) {
			in.Links = []store.Link{lk("acme/api", true), lk("ACME/API", true)}
		}, linkErrInvalid},
		{"a target that is not owner/name", func(_ *planWorld, in *linkInput) { in.Links = []store.Link{lk("acme/../api", true)} }, linkErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld()
			in := w.input([]store.Link{lk("acme/api", true)}, "a.go")
			tc.edit(w, &in)
			plan := planLinks(context.Background(), in)
			if len(plan.Entries) == 0 {
				t.Fatal("no entry: a link that can't be checked was dropped")
			}
			for _, e := range plan.Entries {
				if e.Error != tc.want {
					t.Errorf("error = %q; want %q", e.Error, tc.want)
				}
				if strings.Contains(e.Error, "ghs_") || strings.Contains(e.Error, "boom") {
					t.Errorf("the platform's error text reached the entry: %q", e.Error)
				}
				if e.Path == "" || strings.Contains(e.Path, e.Name) {
					t.Errorf("path %q for %q", e.Path, e.Name)
				}
			}
			if len(plan.Clones) != 0 || len(plan.RepoIDs) != 0 {
				t.Errorf("an error entry is cloned: %+v", plan)
			}
		})
	}
}

// An error on one link leaves the others to be cloned.
func TestPlanLinksOneErrorDoesNotStopTheRest(t *testing.T) {
	w := newPlanWorld()
	w.visErr["acme/api"] = errors.New("boom")
	plan := planLinks(context.Background(), w.input([]store.Link{lk("acme/api", true), lk("acme/web", true)}, "a.go"))
	if len(plan.Entries) != 2 || plan.Entries[0].Error == "" || plan.Entries[1].Error != "" {
		t.Fatalf("entries = %+v", plan.Entries)
	}
	// The second link keeps its position: linked-2.
	if len(plan.Clones) != 1 || plan.Clones[0].Dir != "linked-2" || plan.Entries[1].Path != "/work/linked-2" {
		t.Errorf("clones = %+v", plan.Clones)
	}
}

func TestPlanLinksTooManyLinks(t *testing.T) {
	w := newPlanWorld()
	in := w.input([]store.Link{lk("acme/a", true), lk("acme/b", true), lk("acme/c", true), lk("acme/d", true)}, "a.go")
	plan := planLinks(context.Background(), in)
	if len(plan.Entries) != 3 {
		t.Fatalf("%d entries; want 3 (linked-1 to linked-3)", len(plan.Entries))
	}
	for _, e := range plan.Entries {
		if e.Error != linkErrInvalid {
			t.Errorf("entry %+v", e)
		}
	}
}

// A rename's old path is among the paths the caller passes (forge.Paths), so
// moving a watched file out of its folder touches the link.
func TestPlanLinksMatchesARenamesOldPath(t *testing.T) {
	w := newPlanWorld()
	ls := []store.Link{lk("acme/api", true, "render/**")}
	paths := forge.Paths([]forge.FileDiff{{Path: "elsewhere/comment.go", OldPath: "render/comment.go"}})
	if plan := planLinks(context.Background(), w.input(ls, paths...)); len(plan.Entries) != 1 {
		t.Errorf("a file moved out of render/ matched %d links; want 1", len(plan.Entries))
	}
	if plan := planLinks(context.Background(), w.input(ls, "elsewhere/comment.go")); len(plan.Entries) != 0 {
		t.Errorf("without the old path the link matched: %+v", plan.Entries)
	}
}

func TestPlanLinksAllMatchesWithoutFiles(t *testing.T) {
	w := newPlanWorld()
	in := w.input([]store.Link{lk("acme/api", true, "render/**")})
	in.All = true
	if plan := planLinks(context.Background(), in); len(plan.Entries) != 1 || len(plan.Clones) != 1 {
		t.Errorf("plan = %+v", plan)
	}
}

// ---------------------------------------------------------------------------
// impactOpen

func impactResult(t *testing.T, skills ...result.Skill) *result.Result {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", Skills: skills,
		Comment: result.Comment{Markdown: testMarkdown},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := result.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func skillWith(name, verdict string, findings ...result.Finding) result.Skill {
	s := result.Skill{Name: name, Verdict: verdict, Findings: []json.RawMessage{}}
	for _, f := range findings {
		b, _ := json.Marshal(f)
		s.Findings = append(s.Findings, b)
	}
	return s
}

func TestImpactOpen(t *testing.T) {
	blocking := result.Finding{Fingerprint: "fp-1", File: "a.go", Line: 3, Severity: "blocking", Title: "breaks api", Explanation: "x"}
	suggestion := result.Finding{Fingerprint: "fp-2", File: "a.go", Line: 4, Severity: "suggestion", Title: "meh", Explanation: "x"}
	for _, tc := range []struct {
		name   string
		skills []result.Skill
		ds     []render.Dismissal
		want   bool
	}{
		{"a failed impact", []result.Skill{skillWith("impact", "failed")}, nil, true},
		{"an open blocking finding", []result.Skill{skillWith("impact", "has_blocking_issues", blocking)}, nil, true},
		{"a dismissed blocking finding", []result.Skill{skillWith("impact", "has_blocking_issues", blocking)}, []render.Dismissal{{Fingerprint: "fp-1", Severity: "blocking", By: "bob", Reason: "ok"}}, false},
		{"a dismissed suggestion does not close a blocking finding", []result.Skill{skillWith("impact", "has_blocking_issues", blocking)}, []render.Dismissal{{Fingerprint: "fp-1", Severity: "suggestion", By: "bob"}}, true},
		{"a clean impact", []result.Skill{skillWith("impact", "no_blocking_issues")}, nil, false},
		{"only suggestions", []result.Skill{skillWith("impact", "no_blocking_issues", suggestion)}, nil, false},
		{"another skill is blocking", []result.Skill{skillWith("security", "has_blocking_issues", blocking), skillWith("impact", "no_blocking_issues")}, nil, false},
		{"another skill failed", []result.Skill{skillWith("security", "failed")}, nil, false},
		{"no impact skill", []result.Skill{skillWith("security", "no_blocking_issues")}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := impactOpen(impactResult(t, tc.skills...), tc.ds); got != tc.want {
				t.Errorf("impactOpen = %v; want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// linkedRepos: the glue

// linkForge is a GitHub forge that can compare commits, with the two lookups
// a link needs.
type linkForge struct {
	forge.Forge
	forge.Inline
	world     *planWorld
	changed   []string
	changeErr error
	// compared records ChangedSince's from and to.
	compared [][2]string
}

func (f *linkForge) Visibility(ctx context.Context, repo string) (forge.Visibility, error) {
	return f.world.Visibility(ctx, repo)
}

func (f *linkForge) LinkTarget(ctx context.Context, repo string) (githubforge.Target, error) {
	return f.world.Target(ctx, repo)
}

func (f *linkForge) ChangedSince(_ context.Context, _ int64, from, to string) ([]string, error) {
	f.compared = append(f.compared, [2]string{from, to})
	return f.changed, f.changeErr
}

type linkedFixture struct {
	d        *dispatcher
	forge    *linkForge
	plat     Platform
	repo     settings.Effective
	project  store.Project
	mr       forge.Change
	last     store.Run
	haveLast bool
	in       *inputs.Inputs
}

func newLinkedFixture(t *testing.T) *linkedFixture {
	t.Helper()
	lf := &linkForge{world: newPlanWorld(), changed: []string{"render/comment.go"}}
	return &linkedFixture{
		d:     &dispatcher{e: &Engine{Config: &config.Config{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), run: store.Run{HeadSHA: sha('2'), TargetBranch: "main", MR: store.MRKey{ProjectID: ghRepoID}}},
		forge: lf,
		plat:  Platform{Kind: config.PlatformGitHub, Forge: lf},
		repo: settings.Effective{
			LinkedReposEnabled: true,
			LinkedRepos:        []store.Link{lk("acme/api", true, "render/**")},
		},
		project: store.Project{Path: planSource, HTTPURLToRepo: "https://github.com/acme/gadgets.git", InstallationID: 55},
		mr:      forge.Change{BaseSHA: sha('b'), HeadSHA: sha('2')},
		in:      &inputs.Inputs{Skills: map[string][]byte{"security": []byte("Look.\n")}},
	}
}

func (f *linkedFixture) plan() linkPlan {
	return f.d.linkedRepos(context.Background(), f.plat, f.repo, f.project, f.mr, f.last, f.haveLast, f.in)
}

func TestLinkedReposOffChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *linkedFixture)
	}{
		{"the setting is off", func(f *linkedFixture) { f.repo.LinkedReposEnabled = false }},
		{"there are no links", func(f *linkedFixture) { f.repo.LinkedRepos = nil }},
		{"the platform is GitLab", func(f *linkedFixture) { f.plat.Kind = config.PlatformGitLab }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			tc.edit(f)
			plan := f.plan()
			if len(plan.Entries) != 0 || len(plan.Clones) != 0 || len(plan.Matched) != 0 {
				t.Errorf("plan = %+v; want none", plan)
			}
			if f.forge.world.calls != 0 || len(f.forge.compared) != 0 {
				t.Errorf("%d platform calls and %d compares with the feature off", f.forge.world.calls, len(f.forge.compared))
			}
		})
	}
}

// Nothing matched: nothing happens below the match, no visibility is read.
func TestLinkedReposNothingMatched(t *testing.T) {
	f := newLinkedFixture(t)
	f.forge.changed = []string{"cmd/main.go"}
	if plan := f.plan(); len(plan.Entries) != 0 || f.forge.world.calls != 0 {
		t.Errorf("plan = %+v, %d calls", plan, f.forge.world.calls)
	}
}

func TestLinkedReposFullReviewComparesFromTheMergeBase(t *testing.T) {
	f := newLinkedFixture(t)
	f.last, f.haveLast = store.Run{HeadSHA: sha('1')}, true
	f.last.Result = mustResultJSON(t, skillWith("security", "no_blocking_issues"))
	plan := f.plan()
	if len(plan.Entries) != 1 || plan.Entries[0].Error != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if want := [][2]string{{sha('b'), sha('2')}}; !slices.Equal(f.forge.compared, want) {
		t.Errorf("compared %v; want %v", f.forge.compared, want)
	}
}

// An incremental review counts the new commits' files only.
func TestIncrementalMatchesNewCommitsOnlyOffline(t *testing.T) {
	f := newLinkedFixture(t)
	f.in.Carried = []byte(`{"security":["old.go"]}`)
	f.last, f.haveLast = store.Run{HeadSHA: sha('1')}, true
	f.last.Result = mustResultJSON(t, skillWith("security", "no_blocking_issues"))
	f.forge.changed = []string{"cmd/main.go"} // the new commits touch nothing watched
	if plan := f.plan(); len(plan.Entries) != 0 {
		t.Errorf("plan = %+v; want none", plan)
	}
	if want := [][2]string{{sha('1'), sha('2')}}; !slices.Equal(f.forge.compared, want) {
		t.Errorf("compared %v; want %v (the earlier review's head to this one)", f.forge.compared, want)
	}
}

func mustResultJSON(t *testing.T, skills ...result.Skill) []byte {
	t.Helper()
	b, err := json.Marshal(result.Result{
		SchemaVersion: result.SchemaVersion, Status: "completed", Skills: skills,
		Comment: result.Comment{Markdown: testMarkdown},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The rule for a later push: an open impact check makes every link count,
// whatever the push touched, and no file list is needed.
func TestLinkedReposOpenImpactMatchesEverything(t *testing.T) {
	f := newLinkedFixture(t)
	f.repo.LinkedRepos = []store.Link{lk("acme/api", true, "render/**"), lk("acme/web", false, "scope/**")}
	f.forge.changed = []string{"cmd/main.go"}
	f.last, f.haveLast = store.Run{HeadSHA: sha('1')}, true
	f.last.Result = mustResultJSON(t, skillWith("impact", "failed"))
	plan := f.plan()
	if len(plan.Entries) != 2 {
		t.Fatalf("plan = %+v; want both links", plan)
	}
	if len(f.forge.compared) != 0 {
		t.Errorf("the changed files were listed though every link counts: %v", f.forge.compared)
	}
}

// A result that can't be read counts as open.
func TestLinkedReposUnreadableLastResultIsOpen(t *testing.T) {
	f := newLinkedFixture(t)
	f.forge.changed = []string{"cmd/main.go"}
	f.last, f.haveLast = store.Run{HeadSHA: sha('1'), Result: []byte(`{"schema_version":`)}, true
	if plan := f.plan(); len(plan.Entries) != 1 {
		t.Errorf("plan = %+v; want the link", plan)
	}
}

// Files that can't be listed match every link.
func TestLinkedReposChangedFilesUnavailableMatchesEverything(t *testing.T) {
	f := newLinkedFixture(t)
	f.forge.changeErr = errors.New("not an ancestor")
	if plan := f.plan(); len(plan.Entries) != 1 {
		t.Errorf("plan = %+v; want the link", plan)
	}
	// A platform that can't compare commits at all.
	f = newLinkedFixture(t)
	f.plat.Forge = &noInlineForge{world: f.forge.world}
	if plan := f.plan(); len(plan.Entries) != 1 {
		t.Errorf("plan = %+v; want the link", plan)
	}
}

// noInlineForge can read visibilities but not compare commits.
type noInlineForge struct {
	forge.Forge
	world *planWorld
}

func (n *noInlineForge) Visibility(ctx context.Context, repo string) (forge.Visibility, error) {
	return n.world.Visibility(ctx, repo)
}

// The entry's text is fixed, but the operator can tell why a read failed: the
// cause goes to the log through Cause, once for each failed entry.
func TestPlanLinksTellsTheCause(t *testing.T) {
	boom := errors.New("GET /repos/acme/api: 502 bad gateway")
	for _, tc := range []struct {
		name string
		edit func(w *planWorld)
		want string
	}{
		{"visibility read", func(w *planWorld) { w.visErr["acme/api"] = boom }, "502 bad gateway"},
		{"source read", func(w *planWorld) { w.visErr[planSource] = boom }, "502 bad gateway"},
		{"not readable", func(w *planWorld) { w.visErr["acme/api"] = forge.ErrSourceNotFound }, forge.ErrSourceNotFound.Error()},
		{"refused", func(w *planWorld) { w.vis[planSource] = forge.VisPublic }, "must be as open"},
		{"target lookup", func(w *planWorld) { w.targetErr["acme/api"] = boom }, "502 bad gateway"},
		{"another installation", func(w *planWorld) { w.targets["acme/api"] = githubforge.Target{RepoID: 11, InstallationID: 99} }, "another installation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPlanWorld()
			tc.edit(w)
			in := w.input([]store.Link{lk("acme/api", true)}, "a.go")
			in.All = true
			var links, causes []string
			in.Cause = func(link string, err error) { links, causes = append(links, link), append(causes, err.Error()) }

			plan := planLinks(context.Background(), in)

			if len(plan.Entries) != 1 || plan.Entries[0].Error == "" {
				t.Fatalf("entries = %+v; want one with an error", plan.Entries)
			}
			if len(causes) != 1 || links[0] != "acme/api" || !strings.Contains(causes[0], tc.want) {
				t.Errorf("causes = %q for %q; want one containing %q", causes, links, tc.want)
			}
			if strings.Contains(plan.Entries[0].Error, "502") {
				t.Errorf("the entry shows the cause: %q", plan.Entries[0].Error)
			}
		})
	}

	// A link that works has no cause.
	w := newPlanWorld()
	in := w.input([]store.Link{lk("acme/api", true)}, "a.go")
	in.All = true
	in.Cause = func(string, error) { t.Error("a cause for a working link") }
	if plan := planLinks(context.Background(), in); plan.Entries[0].Error != "" {
		t.Errorf("entries = %+v", plan.Entries)
	}
}
